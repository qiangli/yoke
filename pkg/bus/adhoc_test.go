package bus

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

// adHocEnv is a native agent session with nothing declared: a harness is
// detected, no principal, no instance, no agent name. Each test states the
// session key it has. It returns the store and the captured naming notice.
func adHocEnv(t *testing.T, tool string) (*fleet.InstanceStore, *bytes.Buffer) {
	t.Helper()
	boardInTempHome(t)
	for _, env := range []string{"BASHY_AGENT", "WEAVE_AGENT", "BASHY_AGENT_ID", "WEAVE_CONDUCTOR",
		"BASHY_SESSION_CLAIM", AdHocIdentityEnv} {
		t.Setenv(env, "")
	}
	store := fleet.NewInstanceStore(t.TempDir())
	priorStore, priorDetect, priorSession, priorAncestor, priorNotice :=
		InstanceStoreFn, DetectHarness, CurrentSessionClaim, harnessAncestor, AdHocNotice
	priorOn := AdHocIdentity
	AdHocIdentity = true
	t.Cleanup(func() { AdHocIdentity = priorOn })
	notice := &bytes.Buffer{}
	InstanceStoreFn = func() *fleet.InstanceStore { return store }
	DetectHarness = func() (string, bool) { return tool, true }
	CurrentSessionClaim = nil
	AdHocNotice = notice
	t.Cleanup(func() {
		InstanceStoreFn, DetectHarness, CurrentSessionClaim, harnessAncestor, AdHocNotice =
			priorStore, priorDetect, priorSession, priorAncestor, priorNotice
	})
	return store, notice
}

// nextCommand is the next bashy command of the same session: a new process,
// so nothing the previous one put in its own environment is there.
func nextCommand(t *testing.T) {
	t.Helper()
	t.Setenv(principal.InstanceEnv, "")
	t.Setenv("BASHY_SESSION_CLAIM", "")
}

var adHocLabelWant = regexp.MustCompile(`^claude-[0-9a-f]{8}$`)

// A native session whose tool declares a session id is one identity for every
// command it runs, with no name declared: the instance is derived from the id.
func TestAdHocIdentityFromTheVendorSessionID(t *testing.T) {
	store, notice := adHocEnv(t, "claude")
	CurrentSessionClaim = func(string) string { return "sess-1" }

	first, err := BoardIdentity("")
	if err != nil {
		t.Fatalf("BoardIdentity: %v", err)
	}
	id, ok := ExplicitInstanceID(first)
	if !ok {
		t.Fatalf("identity %q is not an instance", first)
	}
	inst, err := store.Get(id)
	if err != nil || !adHocLabelWant.MatchString(inst.Label) || !strings.HasSuffix(inst.Label, id[:8]) {
		t.Fatalf("instance = %+v, %v; want a label claude-<first 8 hex of its UUID>", inst, err)
	}
	if got := notice.String(); !strings.Contains(got, inst.Label) || strings.Count(got, "\n") != 1 {
		t.Fatalf("naming notice = %q, want one line naming %s", got, inst.Label)
	}

	// The next command of the same session resolves to the same instance and
	// is not told its name again.
	nextCommand(t)
	notice.Reset()
	if again, err := BoardIdentity(""); err != nil || again != first {
		t.Fatalf("second command = %q, %v; want %q", again, err, first)
	}
	if notice.Len() != 0 {
		t.Fatalf("the name was announced twice: %q", notice.String())
	}

	// It can author as itself, and the seat it takes carries the session.
	nextCommand(t)
	if actor, err := ResolveAuthoredActor(""); err != nil || actor != inst.MailAddress() {
		t.Fatalf("authored actor = %q, %v; want %q", actor, err, inst.MailAddress())
	}
	if owner, owned := principal.InstanceOwner(inst); !owned || owner.SessionClaim != HashSessionClaim("sess-1") {
		t.Fatalf("seat = %+v owned=%v, want it held under the session claim", owner, owned)
	}

	// Another session of the same tool is another identity.
	nextCommand(t)
	CurrentSessionClaim = func(string) string { return "sess-2" }
	if other, err := BoardIdentity(""); err != nil || other == first {
		t.Fatalf("a second session resolved to %q, %v; want a different instance", other, err)
	}
}

// A tool that declares no session id is keyed on the harness process above
// the command: its pid and start time.
func TestAdHocIdentityFromTheHarnessAncestor(t *testing.T) {
	store, _ := adHocEnv(t, "agy")
	var asked []string
	harnessAncestor = func(names ...string) (int, bool) {
		asked = names
		return os.Getpid(), true
	}

	first, err := BoardIdentity("")
	if err != nil {
		t.Fatalf("BoardIdentity: %v", err)
	}
	if len(asked) == 0 || asked[0] != "agy" {
		t.Fatalf("looked for ancestors named %v, want the tool name first", asked)
	}
	id, _ := ExplicitInstanceID(first)
	inst, err := store.Get(id)
	if err != nil || !strings.HasPrefix(inst.Label, "agy-") {
		t.Fatalf("instance = %+v, %v", inst, err)
	}
	nextCommand(t)
	if again, err := BoardIdentity(""); err != nil || again != first {
		t.Fatalf("second command = %q, %v; want %q", again, err, first)
	}
	// With no vendor session, the process key is the session claim the seat
	// is taken under.
	nextCommand(t)
	if actor, err := ResolveAuthoredActor(""); err != nil || actor != inst.MailAddress() {
		t.Fatalf("authored actor = %q, %v", actor, err)
	}
	if owner, owned := principal.InstanceOwner(inst); !owned || !strings.HasPrefix(owner.SessionClaim, "sha256:") {
		t.Fatalf("seat = %+v owned=%v, want a session claim from the process key", owner, owned)
	}
}

// Nothing to key the session on means nothing is invented: the refusal and
// its guidance stand.
func TestAdHocIdentityNeedsASessionKey(t *testing.T) {
	store, notice := adHocEnv(t, "claude")
	harnessAncestor = func(...string) (int, bool) { return 0, false }

	if got, err := BoardIdentity(""); !errors.Is(err, ErrUnattributed) {
		t.Fatalf("identity = %q, %v; want the unattributed refusal", got, err)
	}
	if all, _ := store.List(); len(all) != 0 || notice.Len() != 0 {
		t.Fatalf("an instance was opened with no session key: %+v %q", all, notice.String())
	}
}

// The ad hoc identity is only for a caller with nothing else: a declared
// agent, a named sender, a non-agent caller and the off switch all keep their
// existing behavior, and none of them opens an instance.
func TestAdHocIdentityLeavesEveryOtherCallerAlone(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"declared agent":          func(t *testing.T) { t.Setenv("BASHY_AGENT", "agent-x") },
		"off switch":              func(t *testing.T) { t.Setenv(AdHocIdentityEnv, "off") },
		"not an agent":            func(t *testing.T) { DetectHarness = func() (string, bool) { return "", false } },
		"no hook":                 func(t *testing.T) { DetectHarness = nil },
		"not a messaging surface": func(t *testing.T) { AdHocIdentity = false },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			store, _ := adHocEnv(t, "claude")
			CurrentSessionClaim = func(string) string { return "sess-1" }
			setup(t)
			if adoptAdHocIdentity("") {
				t.Fatal("an ad hoc identity was adopted")
			}
			if all, _ := store.List(); len(all) != 0 {
				t.Fatalf("an instance was opened: %+v", all)
			}
		})
	}
	t.Run("named sender", func(t *testing.T) {
		store, _ := adHocEnv(t, "claude")
		CurrentSessionClaim = func(string) string { return "sess-1" }
		if adoptAdHocIdentity("someone") {
			t.Fatal("--as was overridden by an ad hoc identity")
		}
		if all, _ := store.List(); len(all) != 0 {
			t.Fatalf("an instance was opened: %+v", all)
		}
	})
	t.Run("human", func(t *testing.T) {
		adHocEnv(t, "claude")
		DetectHarness = nil
		if got, err := BoardIdentity(""); err != nil || got != "tester" {
			t.Fatalf("a human resolved to %q, %v; want the login name", got, err)
		}
	})
}

// A session whose instance was retired under it gets the next one, not a
// refusal and not the retired mailbox.
func TestAdHocIdentityAfterItsInstanceWasRetired(t *testing.T) {
	store, _ := adHocEnv(t, "claude")
	CurrentSessionClaim = func(string) string { return "sess-1" }
	first, err := BoardIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ExplicitInstanceID(first)
	if _, err := store.Retire(id, func(fleet.Instance) ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	nextCommand(t)
	second, err := BoardIdentity("")
	if err != nil || second == first {
		t.Fatalf("after retirement = %q, %v; want a new instance", second, err)
	}
	nextCommand(t)
	if third, err := BoardIdentity(""); err != nil || third != second {
		t.Fatalf("the replacement is not stable: %q, %v; want %q", third, err, second)
	}
}

// A family at its cap makes room by retiring its oldest ad hoc instance that
// no live session owns, and never anything else.
func TestAdHocIdentityMakesRoomAtTheFamilyCap(t *testing.T) {
	store, _ := adHocEnv(t, "claude")
	capped := store.WithCap(2)
	InstanceStoreFn = func() *fleet.InstanceStore { return capped }
	session := "sess-1"
	CurrentSessionClaim = func(string) string { return session }

	var ids []string
	for _, s := range []string{"sess-1", "sess-2", "sess-3"} {
		nextCommand(t)
		session = s
		addr, err := BoardIdentity("")
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		id, _ := ExplicitInstanceID(addr)
		ids = append(ids, id)
	}
	// One of the two earlier, idle instances gave way; the newest is active.
	active := 0
	for _, id := range ids[:2] {
		inst, err := capped.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if inst.Active() {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("%d of the two earlier instances are active, want exactly 1", active)
	}
	if inst, err := capped.Get(ids[2]); err != nil || !inst.Active() {
		t.Fatalf("newest instance = %+v, %v; want it active", inst, err)
	}
}

// A native session never retires its own instance, so the next session to
// open one sweeps the ones left behind: ad hoc, unowned and a day old.
func TestAdHocIdentitySweepsStaleInstances(t *testing.T) {
	store, _ := adHocEnv(t, "claude")
	session := "sess-old"
	CurrentSessionClaim = func(string) string { return session }
	old, err := BoardIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	oldID, _ := ExplicitInstanceID(old)

	// Same day: a second session leaves it alone.
	nextCommand(t)
	session = "sess-same-day"
	if _, err := BoardIdentity(""); err != nil {
		t.Fatal(err)
	}
	if inst, err := store.Get(oldID); err != nil || !inst.Active() {
		t.Fatalf("a fresh instance was swept: %+v, %v", inst, err)
	}

	// Two days on, a new session sweeps both earlier ones.
	priorNow := adHocNow
	adHocNow = func() time.Time { return time.Now().Add(48 * time.Hour) }
	t.Cleanup(func() { adHocNow = priorNow })
	nextCommand(t)
	session = "sess-later"
	later, err := BoardIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	laterID, _ := ExplicitInstanceID(later)
	all, _ := store.List()
	for _, inst := range all {
		if want := inst.UUID == laterID; inst.Active() != want {
			t.Fatalf("instance %s active=%v, want %v", inst.Label, inst.Active(), want)
		}
	}
}

// A process that only needs the session's mailbox for one question borrows
// the identity and gives it back: what it serves next sees no identity.
func TestWithAdHocIdentityIsScoped(t *testing.T) {
	adHocEnv(t, "claude")
	CurrentSessionClaim = func(string) string { return "sess-1" }
	AdHocIdentity = false

	var inside string
	WithAdHocIdentity(func() { inside, _ = BoardIdentity("") })
	if _, ok := ExplicitInstanceID(inside); !ok {
		t.Fatalf("inside the scope the reader is %q, want the session's instance", inside)
	}
	if AdHocIdentity || os.Getenv(principal.InstanceEnv) != "" {
		t.Fatalf("the scope leaked: on=%v %s=%q", AdHocIdentity, principal.InstanceEnv, os.Getenv(principal.InstanceEnv))
	}
	if got, err := BoardIdentity(""); !errors.Is(err, ErrUnattributed) {
		t.Fatalf("after the scope the caller resolved to %q, %v; want the refusal", got, err)
	}
}

func TestProcessBaseName(t *testing.T) {
	for in, want := range map[string]string{
		"claude":                       "claude",
		"/usr/local/bin/Claude":        "claude",
		`C:\Users\x\AppData\codex.exe`: "codex",
		"  agy  ":                      "agy",
		"":                             "",
	} {
		if got := processBaseName(in); got != want {
			t.Errorf("processBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}
