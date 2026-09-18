package bus

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/admission"
	"github.com/qiangli/yoke/pkg/room"
)

// joinLive publishes a room card so a subscription's Instance resolves to a
// reachable control socket, the way a live agent session does.
// Uses our OWN pid: room.Members prunes a card whose pid is not alive, and the
// liveness probe is signal 0 — which returns EPERM (not success) for a process we
// do not own. A card claiming pid 1 is therefore pruned as dead on macOS, and the
// test would fail for a reason that has nothing to do with the bus.
func joinLive(t *testing.T, id, ctlSock string) {
	t.Helper()
	if err := room.Join(room.Card{ID: id, CtlSock: ctlSock, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
}

// The turn-boundary hook: the harness reads the buffer on the agent's behalf, at
// the one moment the agent is guaranteed to be listening. This is what closes the
// gap the sidecar exists for — `bus pending` is a channel an agent must CHOOSE to
// read, and the premise is that it cannot reliably choose.
func TestTurnPreambleDeliversAndClears(t *testing.T) {
	isolate(t)
	joinLive(t, "dev-b-session", "/tmp/dev-b.sock")
	subscribe(t, Subscription{
		Subscriber: "dev-b", Topics: []string{"code.api.*"}, Instance: "dev-b-session",
	})
	publish(t, "--topic", "code.api.Foo", "--as", "dev-a", "Foo was renamed")

	if _, err := testSidecar(t, &recorder{}).Once(); err != nil {
		t.Fatal(err)
	}

	block := TurnPreamble("/tmp/dev-b.sock")
	if !strings.Contains(block, "Foo was renamed") {
		t.Fatalf("the turn preamble did not carry the notification:\n%s", block)
	}
	// Cleared, or the agent would see the same notification at every turn forever.
	if again := TurnPreamble("/tmp/dev-b.sock"); again != "" {
		t.Errorf("a second turn re-delivered the notification:\n%s", again)
	}
}

func TestTurnPreambleIncludesHostUnifiedInboxWithoutBusPending(t *testing.T) {
	isolate(t)
	joinLive(t, "dev-b-session", "/tmp/dev-b-unified.sock")
	subscribe(t, Subscription{Subscriber: "dev-b", Instance: "dev-b-session"})
	old := PrepareTurnInbox
	PrepareTurnInbox = func(agent string) PreparedPreamble {
		if agent != "dev-b" {
			t.Fatalf("unified hook agent = %q", agent)
		}
		return NewPreparedPreamble("meet and board input", nil)
	}
	t.Cleanup(func() { PrepareTurnInbox = old })

	if got := TurnPreamble("/tmp/dev-b-unified.sock"); !strings.Contains(got, "meet and board input") {
		t.Fatalf("turn boundary omitted host unified inbox: %q", got)
	}
}

func TestLegacyHostPreambleRemainsExactUntilItemAdapterIsWired(t *testing.T) {
	isolate(t)
	joinLive(t, "legacy-session", "/tmp/legacy.sock")
	subscribe(t, Subscription{Subscriber: "legacy", Instance: "legacy-session"})
	body := "legacy host input " + strings.Repeat("x", admission.DefaultPreviewBytes)
	acked := false
	oldInbox, oldItems := PrepareTurnInbox, PrepareTurnItems
	PrepareTurnItems = nil
	PrepareTurnInbox = func(string) PreparedPreamble {
		return NewPreparedPreamble(body, func() error { acked = true; return nil })
	}
	t.Cleanup(func() { PrepareTurnInbox, PrepareTurnItems = oldInbox, oldItems })

	prepared := PrepareTurnPreamble("/tmp/legacy.sock")
	if err := prepared.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prepared.Text, body) {
		t.Fatalf("legacy snapshot was reduced before a record-scoped adapter existed: %q", prepared.Text)
	}
	if acked {
		t.Fatal("preparing legacy input acknowledged it")
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if !acked {
		t.Fatal("delivered legacy snapshot was not acknowledged")
	}
}

func TestPreparedTurnDoesNotAcknowledgeBeforeSuccessfulInjection(t *testing.T) {
	isolate(t)
	joinLive(t, "prepared-session", "/tmp/prepared.sock")
	subscribe(t, Subscription{Subscriber: "prepared", Topics: []string{"*"}, Instance: "prepared-session"})
	publish(t, "--topic", "x", "--as", "sender", "do not lose me")

	first := PrepareTurnPreamble("/tmp/prepared.sock")
	if !strings.Contains(first.Text, "do not lose me") {
		t.Fatalf("prepared turn omitted input: %q", first.Text)
	}
	if retry := PrepareTurnPreamble("/tmp/prepared.sock"); !strings.Contains(retry.Text, "do not lose me") {
		t.Fatal("preparing without injection consumed the input")
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if after := PrepareTurnPreamble("/tmp/prepared.sock"); after.Text != "" {
		t.Fatalf("committed turn remained unread: %q", after.Text)
	}
}

func TestTurnAdmissionBoundsLargeMixedBatchAndKeepsOmissionsUnread(t *testing.T) {
	isolate(t)
	joinLive(t, "bounded-session", "/tmp/bounded.sock")
	subscribe(t, Subscription{Subscriber: "bounded", Topics: []string{"*"}, Instance: "bounded-session"})
	for i := 0; i < 32; i++ {
		body := "routine-" + strconv.Itoa(i) + " " + strings.Repeat("界", 300)
		publish(t, "--topic", "status", "--as", "sender", body)
	}
	if err := Publish(Notification{Principal: "owner", To: "bounded", Topic: "ownership", Body: "BLOCKED ownership changed; stop"}); err != nil {
		t.Fatal(err)
	}

	prepared := PrepareTurnPreamble("/tmp/bounded.sock")
	if err := prepared.Err(); err != nil {
		t.Fatal(err)
	}
	if len(prepared.Text) > DefaultTurnAdmissionBytes {
		t.Fatalf("preamble is %d bytes", len(prepared.Text))
	}
	if !strings.Contains(prepared.Text, "BLOCKED ownership changed") || !strings.Contains(prepared.Text, admission.OverflowSchemaVersion) {
		t.Fatalf("urgent/overflow projection missing:\n%s", prepared.Text)
	}
	if prepared.AdmissionReport().UnrepresentedItems == 0 {
		t.Fatal("large batch unexpectedly had no unrepresented records")
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	retry := PrepareTurnPreamble("/tmp/bounded.sock")
	if err := retry.Err(); err != nil {
		t.Fatal(err)
	}
	if retry.AdmissionReport().InputItems == 0 {
		t.Fatal("committing represented headers silently consumed omitted bodies")
	}
}

func TestCombinedHostAndBusAdmissionSharesOneBudgetAndExactAcks(t *testing.T) {
	isolate(t)
	joinLive(t, "combined-session", "/tmp/combined.sock")
	subscribe(t, Subscription{Subscriber: "combined", Topics: []string{"*"}, Instance: "combined-session"})
	publish(t, "--topic", "routine", "--as", "sender", strings.Repeat("b", 900))

	hostAcked := false
	oldItems := PrepareTurnItems
	PrepareTurnItems = func(agent string) ([]admission.Item, error) {
		return []admission.Item{{
			Source: "meet", ID: "m1", Priority: admission.PriorityUrgent,
			Topic: "CONFLICT", To: agent, Body: strings.Repeat("h", 900),
			ArtifactRef: "bashy inbox --id meet:m1 --peek", OverflowRef: "bashy inbox --peek",
			Acknowledge: func() error { hostAcked = true; return nil },
		}}, nil
	}
	t.Cleanup(func() { PrepareTurnItems = oldItems })

	prepared := PrepareTurnPreamble("/tmp/combined.sock")
	if err := prepared.Err(); err != nil {
		t.Fatal(err)
	}
	if len(prepared.Text) > DefaultTurnAdmissionBytes || !strings.Contains(prepared.Text, "meet:m1") {
		t.Fatalf("combined projection:\n%s", prepared.Text)
	}
	if hostAcked {
		t.Fatal("prepare acknowledged host input")
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if !hostAcked {
		t.Fatal("represented host header was not acknowledged")
	}
}

func TestRenderFailureAndRetryAdvanceNoCursor(t *testing.T) {
	isolate(t)
	joinLive(t, "failure-session", "/tmp/failure.sock")
	subscribe(t, Subscription{Subscriber: "failure", Instance: "failure-session"})
	if err := Publish(Notification{Principal: "security", To: "failure", Topic: "security", Body: "STOP " + strings.Repeat("x", 600)}); err != nil {
		t.Fatal(err)
	}

	oldItems := PrepareTurnItems
	PrepareTurnItems = func(string) ([]admission.Item, error) {
		return []admission.Item{{Source: "host", ID: "bad", Priority: admission.PriorityUrgent, Body: strings.Repeat("z", 600)}}, nil
	}
	failed := PrepareTurnPreamble("/tmp/failure.sock")
	if failed.Err() == nil {
		t.Fatal("urgent record without a retrieval reference did not fail")
	}
	if err := failed.Commit(); err == nil {
		t.Fatal("failed render commit unexpectedly succeeded")
	}
	PrepareTurnItems = nil
	t.Cleanup(func() { PrepareTurnItems = oldItems })

	retry := PrepareTurnPreamble("/tmp/failure.sock")
	if err := retry.Err(); err != nil || retry.AdmissionReport().InputItems != 1 || !strings.Contains(retry.Text, "bus:") {
		t.Fatalf("retry lost unread input: err=%v\n%s", err, retry.Text)
	}
}

// A session nobody subscribed for must get nothing. Subscription is opt-in per
// agent — the design is explicit that auto-subscribing everything to everything
// is how a bus makes a fleet worse.
func TestTurnPreambleIsEmptyWithoutASubscription(t *testing.T) {
	isolate(t)
	joinLive(t, "lonely-session", "/tmp/lonely.sock")
	publish(t, "--topic", "code.api.Foo", "--as", "dev-a", "nobody asked")
	if _, err := testSidecar(t, &recorder{}).Once(); err != nil {
		t.Fatal(err)
	}
	if block := TurnPreamble("/tmp/lonely.sock"); block != "" {
		t.Errorf("an unsubscribed session was handed notifications:\n%s", block)
	}
}

// Sessions are matched by control socket, so one session must never be handed
// another's notifications — the failure a name-based match invites when a name is
// reused across runs.
func TestTurnPreambleDoesNotCrossSessions(t *testing.T) {
	isolate(t)
	joinLive(t, "a-session", "/tmp/a.sock")
	joinLive(t, "b-session", "/tmp/b.sock")
	subscribe(t, Subscription{Subscriber: "a", Topics: []string{"*"}, Instance: "a-session"})
	subscribe(t, Subscription{Subscriber: "b", Topics: []string{"*"}, Instance: "b-session"})
	publish(t, "--topic", "x", "--as", "p", "for both")
	if _, err := testSidecar(t, &recorder{}).Once(); err != nil {
		t.Fatal(err)
	}

	if a := TurnPreamble("/tmp/a.sock"); !strings.Contains(a, "for both") {
		t.Errorf("session a got nothing:\n%s", a)
	}
	// b has its own copy — per-subscriber buffers, so a's read did not consume it.
	if b := TurnPreamble("/tmp/b.sock"); !strings.Contains(b, "for both") {
		t.Errorf("session a's read consumed session b's copy:\n%s", b)
	}
	// An unrelated socket matches nothing.
	if c := TurnPreamble("/tmp/nobody.sock"); c != "" {
		t.Errorf("an unknown socket was handed notifications:\n%s", c)
	}
}

// The block goes FIRST: a notification is context for the instruction that
// follows, and appending it would have the agent commit to an approach and only
// then learn the ground moved.
func TestPrependPutsNotificationsBeforeTheMessage(t *testing.T) {
	isolate(t)
	joinLive(t, "s", "/tmp/s.sock")
	subscribe(t, Subscription{Subscriber: "dev", Topics: []string{"*"}, Instance: "s"})
	publish(t, "--topic", "code.api.Foo", "--as", "dev-a", "Foo was renamed")
	if _, err := testSidecar(t, &recorder{}).Once(); err != nil {
		t.Fatal(err)
	}

	got := Prepend("/tmp/s.sock", "now fix Foo")
	if !strings.HasSuffix(got, "now fix Foo") {
		t.Errorf("the caller's message must come last:\n%s", got)
	}
	if strings.Index(got, "Foo was renamed") > strings.Index(got, "now fix Foo") {
		t.Errorf("the notification must come first:\n%s", got)
	}
}

// An empty or unmatched socket must return the text untouched — the hook is on
// the hot path of every steer and may never mangle a message or block one.
func TestPrependIsInertWithoutNotifications(t *testing.T) {
	isolate(t)
	for _, sock := range []string{"", "/tmp/unknown.sock"} {
		if got := Prepend(sock, "just the message"); got != "just the message" {
			t.Errorf("Prepend(%q) altered the text: %q", sock, got)
		}
	}
}
