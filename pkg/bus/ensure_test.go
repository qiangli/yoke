package bus

import (
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func busInTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// Explicit coordination-store overrides take precedence over HOME, including
	// those used by isolated repository gates. Reset them for every test.
	boardInTempHome(t)
	if room.Dir() == "" {
		t.Skip("room store unavailable")
	}
}

// THE BUG R0 FIXES. Matches delivers a 1:1 notification only when a STORED
// subscription carries To:<name>. Without one, a DM matches nothing and reaches
// nobody — durable and undelivered.
func TestEnsureSubscription_MakesADMDeliverable(t *testing.T) {
	busInTempHome(t)

	dm := room.Event{Type: room.EventNotify, To: "claude-opus5", Body: "hello"}
	if (Subscription{Subscriber: "claude-opus5"}).Matches(dm) {
		t.Fatal("precondition: a subscription with no To must not match a DM")
	}

	created, err := EnsureSubscription("claude-opus5")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("the first call must create an inbox")
	}
	sub, err := LoadSubscription("claude-opus5")
	if err != nil {
		t.Fatal(err)
	}
	if !sub.Matches(dm) {
		t.Fatalf("a reconciled agent must be reachable by name, got %+v", sub)
	}
}

// AN INBOX, NEVER A DOORBELL. Auto-subscribe must not hand out the power to
// break into a running turn — that is granted by name, and widening it as a
// side effect of "make agents reachable" would turn an inbox into an attack
// surface.
func TestEnsureSubscription_GrantsNoInterruptRights(t *testing.T) {
	busInTempHome(t)
	if _, err := EnsureSubscription("codex-gpt5.6-sol"); err != nil {
		t.Fatal(err)
	}
	sub, err := LoadSubscription("codex-gpt5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.InterruptFrom) != 0 {
		t.Fatalf("a default inbox must grant interrupt rights to NOBODY, got %v", sub.InterruptFrom)
	}
	if len(sub.Topics) != 0 {
		t.Fatalf("a default inbox is not a subscription to the firehose, got %v", sub.Topics)
	}
	if sub.To != "codex-gpt5.6-sol" {
		t.Fatalf("To = %q, want the agent's own name", sub.To)
	}
}

// An operator who tuned a subscription has expressed a policy. A reconciliation
// that reset it would be a security regression disguised as a repair.
func TestEnsureSubscription_NeverOverwrites(t *testing.T) {
	busInTempHome(t)
	tuned := Subscription{
		Subscriber:    "claude-opus5",
		To:            "claude-opus5",
		InterruptFrom: []string{"steward"},
		MaxPerMin:     11,
	}
	if err := SaveSubscription(tuned); err != nil {
		t.Fatal(err)
	}
	created, err := EnsureSubscription("claude-opus5")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("an existing subscription must not be recreated")
	}
	got, err := LoadSubscription("claude-opus5")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.InterruptFrom) != 1 || got.InterruptFrom[0] != "steward" || got.MaxPerMin != 11 {
		t.Fatalf("operator policy was clobbered: %+v", got)
	}
}

// Reconciliation is idempotent and reports only what it had to create — that
// report is what turns "every identity is addressable" into a checkable claim.
func TestReconcileSubscriptions_IsIdempotentAndReports(t *testing.T) {
	busInTempHome(t)
	fleet := []string{"claude-opus5", "codex-gpt5.6-sol", "agy-gemini3.1"}

	created, err := ReconcileSubscriptions(fleet)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 3 {
		t.Fatalf("first sweep created %d, want 3", len(created))
	}
	again, err := ReconcileSubscriptions(fleet)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second sweep must create nothing, got %v", again)
	}
	all, err := Subscriptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("stored %d subscriptions, want 3", len(all))
	}
}

// A bad name must not leave the rest of the fleet unaddressable.
func TestReconcileSubscriptions_OneBadNameDoesNotAbortTheSweep(t *testing.T) {
	busInTempHome(t)
	created, err := ReconcileSubscriptions([]string{"claude-opus5", "   ", "agy-gemini3.1"})
	if err == nil {
		t.Fatal("the failure must be reported, not swallowed")
	}
	if len(created) != 2 {
		t.Fatalf("the good names must still be reconciled, created %v", created)
	}
}

// A new inbox opens at the HEAD. Starting at zero would hand its agent every
// notification the host has ever emitted the moment it first connects, as if it
// were new.
func TestEnsureSubscription_OpensAtTheTimelineHead(t *testing.T) {
	busInTempHome(t)
	for range 3 {
		if err := room.Emit(room.Event{Type: room.EventNotify, Topic: "noise", Body: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := EnsureSubscription("claude-opus5"); err != nil {
		t.Fatal(err)
	}
	sub, err := LoadSubscription("claude-opus5")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Since == 0 {
		t.Fatal("a new inbox must open at the head, or the agent is handed the whole backlog as news")
	}
}

func TestBindInstanceMakesDefaultInboxReachableByControlSocket(t *testing.T) {
	isolate(t)
	joinLive(t, "alice-session", "/tmp/alice-bound.sock")
	release, err := BindInstance("alice", "alice-session")
	if err != nil {
		t.Fatal(err)
	}
	if sub, ok := subscriberForCtlSock("/tmp/alice-bound.sock"); !ok || sub != "alice" {
		t.Fatalf("bound socket resolved to %q ok=%v", sub, ok)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, ok := subscriberForCtlSock("/tmp/alice-bound.sock"); ok {
		t.Fatal("released live inbox binding still resolves")
	}
}
