package bus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// relaySpy installs a relay that records whether it was consulted.
func relaySpy(t *testing.T) *[]string {
	t.Helper()
	var asked []string
	prev := RemoteResolve
	RemoteResolve = func(target string) (RemoteRoute, error) {
		asked = append(asked, target)
		return RemoteRoute{Participant: target + "@elsewhere", Host: "elsewhere", Session: "s"}, nil
	}
	t.Cleanup(func() { RemoteResolve = prev })
	return &asked
}

func TestSend_UnknownUUIDFailsClosedAndNeverRelays(t *testing.T) {
	isolate(t)
	instStore(t)
	asked := relaySpy(t)
	before := boardLen(t)
	_, err := Send(SendRequest{From: "tester", To: "00000000-0000-4000-8000-000000000000", Topic: "mb", Body: "x"})
	if err == nil || !Refusal(err) || !strings.Contains(err.Error(), "bashy instance list") {
		t.Fatalf("unknown UUID: %v", err)
	}
	if len(*asked) != 0 || boardLen(t) != before {
		t.Errorf("relay asked %v / board grew — an unknown UUID must fail before any route", *asked)
	}
}

func TestSend_UnreadableStoreFailsClosedAndNeverRelays(t *testing.T) {
	isolate(t)
	bad := filepath.Join(t.TempDir(), "instances")
	if err := os.WriteFile(bad, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := InstanceStoreFn
	InstanceStoreFn = func() *fleet.InstanceStore { return fleet.NewInstanceStore(bad) }
	t.Cleanup(func() { InstanceStoreFn = prev })
	asked := relaySpy(t)
	before := boardLen(t)

	for _, to := range []string{"esme", "11111111-1111-4111-8111-111111111111"} {
		_, err := Send(SendRequest{From: "tester", To: to, Topic: "mb", Body: "x"})
		var re *RecipientError
		if !errors.As(err, &re) || re.Reason != ReasonStore || !Refusal(err) || LegacyName(err) {
			t.Fatalf("%s: store failure = %v, want a definitive %q refusal", to, err, ReasonStore)
		}
	}
	if len(*asked) != 0 || boardLen(t) != before {
		t.Errorf("a validation failure degraded to relay %v / wrote to the board", *asked)
	}
}

func TestSend_UnresolvedBareNameStillMayRelay(t *testing.T) {
	isolate(t)
	instStore(t)
	asked := relaySpy(t)
	RemoteSend = func(RemoteMessage) (RemoteReceipt, error) { return RemoteReceipt{}, nil }
	t.Cleanup(func() { RemoteSend = nil })
	res, err := Send(SendRequest{From: "tester", To: "colleague", Topic: "mb", Body: "hi"})
	if err != nil || res.Kind != SendRemote || len(*asked) != 1 {
		t.Fatalf("legitimate remote name must keep relaying: %+v %v (asked %v)", res, err, *asked)
	}
}

func TestSenderParty_AmbientInstanceSnapshot(t *testing.T) {
	isolate(t)
	s := instStore(t)
	me := openEsme(t, s, "Esme")
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_INSTANCE", me.UUID)
	t.Setenv(SelectedBindingEnv, "claude:opus5")

	// Spelled by label (what --as / the TUI passes), by UUID, or empty: the
	// session's own instance is speaking.
	for _, from := range []string{"Esme", "esme", me.UUID, ""} {
		p := SenderParty(from)
		if p == nil || p.UUID != me.UUID || p.Label != "Esme" || p.FamilyID != me.FamilyID ||
			p.Policy != "single" || len(p.Bindings) != 1 || p.Selected != "claude:opus5" {
			t.Errorf("SenderParty(%q) = %+v", from, p)
		}
	}
	// An explicit other identity never inherits the ambient instance.
	if p := SenderParty("someone-else"); p != nil {
		t.Errorf("--as someone-else inherited the ambient instance: %+v", p)
	}
	// A selection outside the frozen set is not recorded.
	t.Setenv(SelectedBindingEnv, "glm:4.9")
	if p := SenderParty("Esme"); p == nil || p.Selected != "" {
		t.Errorf("out-of-set binding recorded: %+v", p)
	}

	if _, err := Send(SendRequest{From: "Esme", Topic: "mb", Body: "broadcast"}); err != nil {
		t.Fatal(err)
	}
	p := lastPost(t)
	if p.FromParty == nil || p.FromParty.UUID != me.UUID {
		t.Fatalf("stored from_party = %+v", p.FromParty)
	}
	// The snapshot is fixed: the label is released and reused, the old post
	// still says who it was sent by.
	retire(t, s, me.UUID)
	fresh := openEsme(t, s, "Esme")
	if fresh.UUID == me.UUID {
		t.Fatal("uuid reused")
	}
	if got := lastPost(t); got.FromParty.UUID != me.UUID || got.FromParty.Label != "Esme" {
		t.Errorf("old sender snapshot rewritten: %+v", got.FromParty)
	}
}

func TestSend_DefaultRecipientHandleResolvesOnceToUUID(t *testing.T) {
	isolate(t)
	s := instStore(t)
	old := openEsme(t, s, "Esme-2")
	if _, err := Send(SendRequest{From: "tester", To: "Esme-2", Topic: "mb", Body: "queued"}); err != nil {
		t.Fatal(err)
	}
	queued := lastPost(t)
	retire(t, s, old.UUID)
	fresh := openEsme(t, s, "Esme-2")

	if queued.To != "instance/"+old.UUID || !queued.Directed(old.UUID) || queued.Directed(fresh.UUID) {
		t.Fatalf("queued mail follows the reused label: %+v", queued)
	}
	if _, err := Send(SendRequest{From: "tester", To: "Esme-2", Topic: "mb", Body: "new"}); err != nil {
		t.Fatal(err)
	}
	if got := lastPost(t); got.To != "instance/"+fresh.UUID || got.ToParty.UUID != fresh.UUID {
		t.Errorf("new send = %+v, want the new occupant's UUID", got)
	}
}

func TestRoleReaderAuthorizer_HolderOnlyAndVacancyRetained(t *testing.T) {
	isolate(t)
	instStore(t)
	t.Cleanup(func() { RoleReaderAuthorizer = nil })
	withHostRoles(t, HostRole{Label: "conductor:22", Topic: "conductor.22", Holder: "claude-a"})
	p := Post{To: "conductor.22", Body: "x", Seq: 1}

	// Unwired role mail must fail closed.
	RoleReaderAuthorizer = nil
	if p.Directed("anyone") || p.Directed(p.To) {
		t.Fatal("unwired role mail was directed to a reader")
	}
	RoleReaderAuthorizer = AuthorizeByHolder
	if !p.Directed("claude-a") || p.Directed("intruder") || p.Directed("") {
		t.Error("only the current holder may be directed")
	}
	withHostRoles(t, HostRole{Label: "conductor:22", Topic: "conductor.22", Holder: "codex-b"})
	if p.Directed("claude-a") || !p.Directed("codex-b") {
		t.Error("handoff must re-target the mail to the new holder")
	}
	// Vacant: nobody is directed, and the mail is NOT eligible for archive.
	withHostRoles(t, HostRole{Label: "conductor:22", Topic: "conductor.22"})
	if p.Directed("codex-b") || p.Directed("anyone") {
		t.Error("a vacant seat must direct mail at nobody")
	}
	if canArchivePost(p, nil, time.Now(), 0, newAudienceSnapshot()) {
		t.Error("pending mail to a vacant seat was archived")
	}
}
