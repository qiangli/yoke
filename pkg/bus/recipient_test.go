package bus

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// instStore pins a hermetic instance store and returns it.
func instStore(t *testing.T) *fleet.InstanceStore {
	t.Helper()
	s := fleet.NewInstanceStore(t.TempDir())
	prev := InstanceStoreFn
	InstanceStoreFn = func() *fleet.InstanceStore { return s }
	t.Cleanup(func() { InstanceStoreFn = prev })
	return s
}

func esmeFamily() fleet.Family {
	return fleet.Family{Name: "esme", Display: "Esme", Policy: "single", Bindings: []string{"claude:opus5"}}
}

func openEsme(t *testing.T, s *fleet.InstanceStore, label string) fleet.Instance {
	t.Helper()
	i, err := s.Open(esmeFamily(), fleet.OpenOptions{Label: label})
	if err != nil {
		t.Fatalf("open %s: %v", label, err)
	}
	return i
}

func retire(t *testing.T, s *fleet.InstanceStore, id string) {
	t.Helper()
	if _, err := s.Retire(id, func(fleet.Instance) ([]string, error) { return []string{`{"seq":1}`}, nil }); err != nil {
		t.Fatalf("retire: %v", err)
	}
}

func lastPost(t *testing.T) Post {
	t.Helper()
	posts, err := Posts()
	if err != nil || len(posts) == 0 {
		t.Fatalf("board empty (%v)", err)
	}
	return posts[len(posts)-1]
}

func TestResolveRecipient_NameResolvesToUUIDBeforeAppend(t *testing.T) {
	isolate(t)
	s := instStore(t)
	esme := openEsme(t, s, "Esme")

	res, err := Send(SendRequest{From: "tester", To: "esme", Topic: "mb", Body: "hello"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	p := lastPost(t)
	if want := fleet.InstanceAddressPrefix + esme.UUID; p.To != want {
		t.Fatalf("stored To = %q, want the UUID address %q — a handle must never be stored", p.To, want)
	}
	if p.ToParty == nil || p.ToParty.UUID != esme.UUID || p.ToParty.Label != "Esme" || p.ToParty.Family != "esme" || len(p.ToParty.Bindings) != 1 {
		t.Fatalf("to_party snapshot = %+v", p.ToParty)
	}
	if res.Label != "Esme" {
		t.Errorf("receipt label = %q, want the name the sender can read", res.Label)
	}
	if len(res.Deliveries) != 1 || res.Deliveries[0].Warning == "" {
		t.Fatalf("deliveries = %+v, want one with an honest no-read-evidence warning", res.Deliveries)
	}
	if !strings.Contains(res.Deliveries[0].Warning, "not proof") {
		t.Errorf("warning = %q", res.Deliveries[0].Warning)
	}
}

func TestResolveRecipient_ExplicitUUIDSpellings(t *testing.T) {
	isolate(t)
	s := instStore(t)
	esme := openEsme(t, s, "Esme")
	for _, in := range []string{esme.UUID, "instance/" + esme.UUID, "instance:" + esme.UUID, "dhnt:agent/" + esme.UUID} {
		r, err := ResolveRecipient(in)
		if err != nil || r.Kind != TargetInstance || r.Addr != "instance/"+esme.UUID {
			t.Errorf("ResolveRecipient(%q) = %+v, %v", in, r, err)
		}
	}
	if _, err := ResolveRecipient("00000000-0000-4000-8000-000000000000"); err == nil || !Refusal(err) || LegacyName(err) {
		t.Errorf("unknown UUID must be a definitive refusal (a UUID is not a route), got %v", err)
	}
}

func TestResolveRecipient_AmbiguousFamilyFailsBeforePosting(t *testing.T) {
	isolate(t)
	s := instStore(t)
	t.Setenv(fleet.InstanceCapEnv, "5")
	a := openEsme(t, s, "Ada")
	b := openEsme(t, s, "Bea")

	before := boardLen(t)
	_, err := Send(SendRequest{From: "tester", To: "esme", Topic: "mb", Body: "who"})
	if err == nil {
		t.Fatal("a family matching two instances must not be guessed")
	}
	for _, want := range []string{a.UUID, b.UUID, "--family", "nothing was posted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if boardLen(t) != before {
		t.Error("ambiguous send wrote to the board")
	}
	// A direct label still resolves to exactly that instance.
	if r, err := ResolveRecipient("Bea"); err != nil || r.Party.UUID != b.UUID {
		t.Errorf("label Bea = %+v, %v", r, err)
	}
}

func TestResolveRecipient_RetiredLabelNeverRedirects(t *testing.T) {
	isolate(t)
	s := instStore(t)
	old := openEsme(t, s, "Esme-2")
	retire(t, s, old.UUID)

	before := boardLen(t)
	_, err := Send(SendRequest{From: "tester", To: "Esme-2", Topic: "mb", Body: "x"})
	if err == nil || !Refusal(err) {
		t.Fatalf("retired label: err = %v, want a definitive refusal", err)
	}
	if !strings.Contains(err.Error(), old.UUID) || !strings.Contains(err.Error(), "retired") {
		t.Errorf("error should name the retired UUID: %v", err)
	}
	if _, err := Send(SendRequest{From: "tester", To: old.UUID, Topic: "mb", Body: "x"}); err == nil || !Refusal(err) {
		t.Errorf("retired UUID must be refused, got %v", err)
	}
	if boardLen(t) != before {
		t.Error("retired recipient wrote to the board")
	}
}

// The acceptance case: a message queued for old Esme-2 stays addressed to its
// original UUID after the label is retired and re-issued.
func TestReusedLabel_QueuedMailStaysWithOriginalUUID(t *testing.T) {
	isolate(t)
	s := instStore(t)
	old := openEsme(t, s, "Esme-2")
	if _, err := Send(SendRequest{From: "tester", To: "Esme-2", Topic: "mb", Body: "for the old one"}); err != nil {
		t.Fatal(err)
	}
	queued := lastPost(t)
	retire(t, s, old.UUID)
	fresh := openEsme(t, s, "Esme-2")
	if fresh.UUID == old.UUID {
		t.Fatal("label reuse minted the same UUID")
	}

	got := lastPost(t)
	if got.To != "instance/"+old.UUID || got.ToParty.UUID != old.UUID {
		t.Fatalf("queued post re-addressed: %+v", got)
	}
	if got.Seq != queued.Seq {
		t.Fatal("test bug: post changed")
	}
	if !got.Directed(old.UUID) {
		t.Error("original UUID lost its mail")
	}
	if got.Directed(fresh.UUID) || got.Directed("Esme-2") || got.Directed("esme-2") {
		t.Error("queued mail leaked to the label's new occupant")
	}
	r, err := ResolveRecipient("Esme-2")
	if err != nil || r.Party.UUID != fresh.UUID {
		t.Errorf("new sends to the label must go to the new occupant: %+v %v", r, err)
	}
}

func TestResolveRecipient_RetiredLabelDoesNotFallToReaderCursor(t *testing.T) {
	isolate(t)
	s := instStore(t)
	old := openEsme(t, s, "Zed")
	// A leftover cursor under the old label must not resurrect it.
	if err := MarkSeen("Zed", 1); err != nil {
		t.Fatal(err)
	}
	retire(t, s, old.UUID)
	if _, err := ResolveRecipient("Zed"); err == nil || !Refusal(err) {
		t.Fatalf("retired label resolved through the cursor fallback: %v", err)
	}
}

func TestResolveRecipient_RoleAddressSurvivesHandoff(t *testing.T) {
	prev := RoleReaderAuthorizer
	RoleReaderAuthorizer = AuthorizeByHolder
	t.Cleanup(func() { RoleReaderAuthorizer = prev })
	isolate(t)
	instStore(t)
	withHostRoles(t, HostRole{Label: "conductor:22", Topic: "conductor.22", Holder: "claude-a"})

	if _, err := Send(SendRequest{From: "tester", To: "conductor:22", Topic: "mb", Body: "ping"}); err != nil {
		t.Fatal(err)
	}
	p := lastPost(t)
	if p.To != "conductor.22" || p.ToParty != nil {
		t.Fatalf("role stored as %+v — must be the seat, not a holder", p)
	}
	// Handoff: another principal holds the seat. Same post, still directed.
	withHostRoles(t, HostRole{Label: "conductor:22", Topic: "conductor.22", Holder: "codex-b"})
	if !p.Directed("codex-b") {
		t.Error("handoff lost the mail")
	}
	// Vacant seat: retained, with a warning.
	withHostRoles(t, HostRole{Label: "deputy:sprint-1", Topic: "deputy.sprint-1"})
	res, err := Send(SendRequest{From: "tester", To: "deputy:sprint-1", Topic: "mb", Body: "pending"})
	if err != nil {
		t.Fatalf("vacant seat must retain mail: %v", err)
	}
	if len(res.Deliveries) != 1 || !strings.Contains(res.Deliveries[0].Warning, "vacant") {
		t.Errorf("deliveries = %+v, want a vacant-seat warning", res.Deliveries)
	}
}

func TestResolveRecipient_InvalidRoleFails(t *testing.T) {
	isolate(t)
	instStore(t)
	withHostRoles(t, HostRole{Label: "conductor:22", Topic: "conductor.22", Holder: "a"})
	before := boardLen(t)
	_, err := Send(SendRequest{From: "tester", To: "conductor:99", Topic: "mb", Body: "x"})
	if err == nil || !Refusal(err) || !strings.Contains(err.Error(), "conductor:22") {
		t.Fatalf("invalid role: %v", err)
	}
	if _, err := Send(SendRequest{From: "tester", To: "deputy:nowhere", Topic: "mb", Body: "x"}); err == nil {
		t.Error("deputy:<unknown scope> must fail")
	}
	if boardLen(t) != before {
		t.Error("invalid role wrote to the board")
	}
}

func TestSend_SenderSnapshotForInstanceSender(t *testing.T) {
	isolate(t)
	s := instStore(t)
	me := openEsme(t, s, "Esme")
	if _, err := Send(SendRequest{From: "instance/" + me.UUID, To: "steward", Topic: "mb", Body: "x"}); err == nil {
		// no steward role registered: unresolved is expected; use broadcast instead
		t.Fatal("expected unresolved steward")
	}
	if _, err := Send(SendRequest{From: "instance/" + me.UUID, Topic: "mb", Body: "broadcast"}); err != nil {
		t.Fatal(err)
	}
	p := lastPost(t)
	if p.FromParty == nil || p.FromParty.UUID != me.UUID || p.FromParty.Label != "Esme" {
		t.Fatalf("from_party = %+v", p.FromParty)
	}
}

// mb send and ping share one resolver: both go through Send, so the same
// ambiguity and retirement refusals apply identically.
func TestPingAndMBSendShareValidation(t *testing.T) {
	isolate(t)
	s := instStore(t)
	t.Setenv(fleet.InstanceCapEnv, "5")
	openEsme(t, s, "Ada")
	openEsme(t, s, "Bea")
	before := boardLen(t)

	for _, args := range [][]string{{"mb", "send", "esme", "hi"}, {"ping", "esme", "hi"}} {
		var err error
		if args[0] == "ping" {
			cmd := NewPingCmd()
			cmd.SetArgs(args[1:])
			cmd.SetOut(new(strings.Builder))
			cmd.SetErr(new(strings.Builder))
			err = cmd.Execute()
		} else {
			cmd := NewMessageBoardCmd()
			cmd.SetArgs(args[1:])
			cmd.SetOut(new(strings.Builder))
			cmd.SetErr(new(strings.Builder))
			err = cmd.Execute()
		}
		if err == nil || !strings.Contains(err.Error(), "matches 2 live instances") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if boardLen(t) != before {
		t.Error("a refused send wrote to the board")
	}
}

func TestPostDirected_InstanceReaderOnlyByExplicitUUID(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	p := Post{To: "instance/" + id}
	for _, r := range []string{id, "instance/" + id, "instance:" + id, "dhnt:agent/" + id} {
		if !p.Directed(r) {
			t.Errorf("reader %q should be directed", r)
		}
	}
	for _, r := range []string{"Esme", "esme", "tester", ""} {
		if p.Directed(r) {
			t.Errorf("reader %q must not match a UUID address", r)
		}
	}
}
