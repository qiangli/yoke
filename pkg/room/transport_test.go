package room

import (
	"os"
	"strings"
	"testing"
)

// liveCard seats a card owned by this test process, so it is genuinely alive
// rather than merely written down.
func liveCard(t *testing.T, name string, caps ...string) {
	t.Helper()
	if err := Join(Card{
		ID: AgentClaimID(name), Nick: name, Mode: "interactive",
		PID: os.Getpid(), Caps: caps,
	}); err != nil {
		t.Fatalf("join %s: %v", name, err)
	}
}

func TestOwnerTransportClassifiesEachRung(t *testing.T) {
	isolate(t)
	liveCard(t, "managed", CapInboxDelivery)
	liveCard(t, "attached", CapInboxStream)

	for _, tc := range []struct {
		name string
		want OwnerTransport
	}{
		{"managed", TransportManaged},
		{"attached", TransportAttached},
	} {
		got, reason := OwnerTransportFor(tc.name)
		if got != tc.want {
			t.Errorf("OwnerTransportFor(%q) = %v (%s), want %v", tc.name, got, reason, tc.want)
		}
		if !got.Deliverable() {
			t.Errorf("%q classified %v, which must be deliverable", tc.name, got)
		}
		if reason != "" {
			t.Errorf("%q resolved but carried a reason %q; a reason is for refusals", tc.name, reason)
		}
	}
}

// THE LOOPHOLE. `agents track start` publishes a work record with no delivery
// capability. A predicate that asks "is there a live card" passes it, which is
// exactly the state being refused -- and the tool's own refusal text used to
// recommend that command as the fix. Keying on the capability is the whole
// point of the type, so this is the test that must never be deleted.
func TestOwnerTransportRefusesALiveCardThatPromisesNothing(t *testing.T) {
	isolate(t)
	if err := Join(Card{
		ID: AgentClaimID("tracked"), Nick: "tracked", Mode: "weave",
		Task: "externally tracked work", PID: os.Getpid(),
	}); err != nil {
		t.Fatalf("join: %v", err)
	}

	got, reason := OwnerTransportFor("tracked")
	if got != TransportNone {
		t.Fatalf("a card advertising no capability classified %v; it must be none", got)
	}
	// The two None cases have different fixes, so the reason has to tell them
	// apart rather than say "not reachable" for both.
	if !strings.Contains(reason, "advertises no delivery capability") {
		t.Errorf("reason = %q, want it to name the missing capability rather than\n"+
			"blame absence of a session (there IS one)", reason)
	}
	if strings.Contains(reason, "has no live session") {
		t.Errorf("reason = %q misreports a live session as absent", reason)
	}
}

func TestOwnerTransportRefusesUnknownAndEmptyNames(t *testing.T) {
	isolate(t)
	for _, tc := range []struct{ name, wantIn string }{
		{"", "no owner named"},
		{"never-launched", "has no live session"},
	} {
		got, reason := OwnerTransportFor(tc.name)
		if got != TransportNone {
			t.Errorf("OwnerTransportFor(%q) = %v, want none", tc.name, got)
		}
		if !strings.Contains(reason, tc.wantIn) {
			t.Errorf("OwnerTransportFor(%q) reason = %q, want it to contain %q",
				tc.name, reason, tc.wantIn)
		}
	}
}

// A capable card whose process is gone must not answer. Members prunes on read,
// so this asserts the pruning is actually on the path rather than assumed.
func TestOwnerTransportIgnoresADeadProcessEvenWhenCapable(t *testing.T) {
	isolate(t)
	if err := Join(Card{
		ID: AgentClaimID("ghost"), Nick: "ghost", Mode: "interactive",
		PID: 0x7FFFFFFE, Caps: []string{CapInboxDelivery},
	}); err != nil {
		t.Fatalf("join: %v", err)
	}
	got, reason := OwnerTransportFor("ghost")
	if got != TransportNone {
		t.Fatalf("a capable card on a dead PID classified %v (%s); it must be none", got, reason)
	}
}

// Managed outranks attached: an agent holding both must be reported at the rung
// that survives an unattended run, or a caller will pick the weaker path.
func TestOwnerTransportPrefersManagedOverAttached(t *testing.T) {
	isolate(t)
	liveCard(t, "both", CapInboxStream, CapInboxDelivery)
	if got, _ := OwnerTransportFor("both"); got != TransportManaged {
		t.Fatalf("a card with both capabilities = %v, want managed", got)
	}
}

// A pre-contract watcher registered under the raw fleet name instead of the
// claim id. That process must keep working until it exits.
func TestOwnerTransportHonoursTheLegacyBareNameCard(t *testing.T) {
	isolate(t)
	const name = "legacy.watcher"
	if AgentClaimID(name) == name {
		t.Skip("this name needs no claim-id rewrite; the fallback is untestable with it")
	}
	if err := Join(Card{
		ID: name, Nick: name, Mode: "inbox",
		PID: os.Getpid(), Caps: []string{CapInboxStream},
	}); err != nil {
		t.Fatalf("join: %v", err)
	}
	if got, reason := OwnerTransportFor(name); got != TransportAttached {
		t.Fatalf("legacy bare-name card = %v (%s), want attached", got, reason)
	}
}

// The remedies are shared so two refusals cannot drift, and so neither can go
// back to recommending the command that creates the refused state.
func TestOwnerTransportRemediesNeverRecommendTrackStart(t *testing.T) {
	got := OwnerTransportRemedies("scout")
	if strings.Contains(got, "agents track start") {
		t.Fatalf("remedies recommend the very command that mints a card with no\n"+
			"delivery capability:\n%s", got)
	}
	for _, want := range []string{"bashy chat --agent scout", "bashy inbox --as scout --watch"} {
		if !strings.Contains(got, want) {
			t.Errorf("remedies missing %q:\n%s", want, got)
		}
	}
}

func TestOwnerTransportStringsAreStable(t *testing.T) {
	for tr, want := range map[OwnerTransport]string{
		TransportNone: "none", TransportAttached: "attached", TransportManaged: "managed",
	} {
		if got := tr.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(tr), got, want)
		}
	}
}
