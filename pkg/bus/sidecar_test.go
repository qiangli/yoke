package bus

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// recorder is a Deliverer that observes interrupt decisions without a pty.
type recorder struct {
	got  []Pending
	fail error
}

func (r *recorder) Interrupt(_ Subscription, p Pending) error {
	if r.fail != nil {
		return r.fail
	}
	r.got = append(r.got, p)
	return nil
}

func testSidecar(t *testing.T, rec *recorder) *Sidecar {
	t.Helper()
	now := time.Now()
	return &Sidecar{
		Poll:      time.Millisecond,
		Deliverer: rec,
		Now:       func() time.Time { return now },
	}
}

func subscribe(t *testing.T, s Subscription) {
	t.Helper()
	if err := SaveSubscription(s); err != nil {
		t.Fatal(err)
	}
}

// --- topic matching -------------------------------------------------------

// Topics are hierarchical, so a wildcard covers whole dotted SEGMENTS. Matching
// on raw prefix instead would make `code.api.*` also match `code.apiary`,
// silently widening a subscription the author wrote to be precise — which is the
// pollution the design warns is the single biggest failure mode.
func TestTopicMatch(t *testing.T) {
	cases := []struct {
		pattern, topic string
		want           bool
	}{
		{"code.api.Foo", "code.api.Foo", true},
		{"code.api.Foo", "code.api.Bar", false},
		{"code.api.*", "code.api.Foo", true},
		{"code.api.*", "code.api.Foo.renamed", true},
		{"code.api.*", "code.api", true},
		{"code.api.*", "code.apiary", false}, // THE case
		{"code.api.*", "code.apiary.Bee", false},
		{"code.*", "code.api.Foo", true},
		{"*", "anything.at.all", true},
		{"code.api.Foo", "", false},
		{"", "code.api.Foo", false},
	}
	for _, tc := range cases {
		if got := topicMatch(tc.pattern, tc.topic); got != tc.want {
			t.Errorf("topicMatch(%q, %q) = %v, want %v", tc.pattern, tc.topic, got, tc.want)
		}
	}
}

// Session bookkeeping shares the timeline with notifications but is addressed to
// nobody; a subscriber must never be handed it.
func TestSubscriptionIgnoresNonNotifyEvents(t *testing.T) {
	s := Subscription{Topics: []string{"*"}}
	if s.Matches(room.Event{Type: room.EventJoin, Topic: "anything"}) {
		t.Error("a join event matched a bus subscription")
	}
	if !s.Matches(room.Event{Type: room.EventNotify, Topic: "anything"}) {
		t.Error("a notify event did not match")
	}
}

// --- the sidecar loop -----------------------------------------------------

func TestSidecarQueuesMatchingNotifications(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{Subscriber: "dev-a", Topics: []string{"code.api.*"}})

	publish(t, "--topic", "code.api.Foo", "--as", "alice", "Foo changed")
	publish(t, "--topic", "deploy.prod", "--as", "alice", "not yours")

	rec := &recorder{}
	res, err := testSidecar(t, rec).Once()
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued != 1 {
		t.Errorf("queued = %d, want 1", res.Queued)
	}

	items, err := ReadPending("dev-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Body != "Foo changed" {
		t.Fatalf("pending = %+v", items)
	}
	if items[0].Delivery != DeliveryQueued {
		t.Errorf("delivery = %q, want queued", items[0].Delivery)
	}
	if len(rec.got) != 0 {
		t.Error("a queued notification was delivered as an interrupt")
	}
}

// The sidecar must not re-deliver on the next pass, or an agent reading its
// buffer at each turn boundary would see the same notification forever.
func TestSidecarDoesNotRedeliver(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{Subscriber: "dev-a", Topics: []string{"*"}})
	publish(t, "--topic", "x", "--as", "alice", "once")

	sc := testSidecar(t, &recorder{})
	if _, err := sc.Once(); err != nil {
		t.Fatal(err)
	}
	res, err := sc.Once()
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued != 0 {
		t.Errorf("second pass re-delivered %d notification(s)", res.Queued)
	}
	if items, _ := ReadPending("dev-a"); len(items) != 1 {
		t.Errorf("pending has %d items, want 1", len(items))
	}
}

// --- governance -----------------------------------------------------------

// The power to break into an agent's turn is granted BY NAME, never assumed.
// This is the report/author split: everyone may report, only named principals
// may redirect.
func TestInterruptRequiresAnAuthorizedPrincipal(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{
		Subscriber: "dev-a", Topics: []string{"*"},
		Instance:      "dev-a-session",
		InterruptFrom: []string{"steward"},
	})

	publish(t, "--topic", "x", "--as", "randomer", "--priority", "interrupt", "let me in")

	rec := &recorder{}
	res, err := testSidecar(t, rec).Once()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 0 {
		t.Fatal("an unauthorized principal interrupted a running turn")
	}
	if res.Demoted != 1 {
		t.Errorf("demoted = %d, want 1", res.Demoted)
	}

	// DEMOTED, NOT DROPPED. Governance reduces urgency; it does not decide
	// whether the agent gets to know. A dropped notification leaves an agent on
	// stale assumptions and it cannot ask for what it never saw.
	items, _ := ReadPending("dev-a")
	if len(items) != 1 {
		t.Fatalf("an unauthorized interrupt was DROPPED (pending = %+v)", items)
	}
	if items[0].Delivery != DeliveryQueued {
		t.Errorf("delivery = %q, want queued", items[0].Delivery)
	}
	if !strings.Contains(items[0].Demoted, "interrupt_from") {
		t.Errorf("the demotion reason should name the governance rule, got %q", items[0].Demoted)
	}
}

func TestAuthorizedInterruptIsDelivered(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{
		Subscriber: "dev-a", Topics: []string{"*"},
		Instance:      "dev-a-session",
		InterruptFrom: []string{"steward"},
	})
	publish(t, "--topic", "fleet.priority", "--as", "steward", "--priority", "interrupt", "stop, priorities changed")

	rec := &recorder{}
	res, err := testSidecar(t, rec).Once()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("authorized interrupt was not delivered: %+v", rec.got)
	}
	if res.Interrupts != 1 {
		t.Errorf("interrupts = %d, want 1", res.Interrupts)
	}
	// It is ALSO in the buffer: an interrupt the agent handles mid-turn should
	// still be readable at the next boundary, or the record of why it changed
	// course disappears.
	if items, _ := ReadPending("dev-a"); len(items) != 1 || items[0].Delivery != DeliveryInterrupt {
		t.Errorf("pending = %+v", items)
	}
}

// Default-closed: a subscription that names nobody accepts no interrupts.
func TestInterruptFromDefaultsToNobody(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{Subscriber: "dev-a", Topics: []string{"*"}, Instance: "s"})
	publish(t, "--topic", "x", "--as", "steward", "--priority", "interrupt", "hello")

	rec := &recorder{}
	if _, err := testSidecar(t, rec).Once(); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 0 {
		t.Error("an interrupt was honoured with no interrupt_from configured")
	}
	if items, _ := ReadPending("dev-a"); len(items) != 1 {
		t.Error("the notification was dropped instead of demoted")
	}
}

// An interrupt with nowhere to go degrades rather than erroring: the subscriber
// still learns about it at its next turn boundary.
func TestInterruptWithNoInstanceIsDemoted(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{
		Subscriber: "dev-a", Topics: []string{"*"},
		InterruptFrom: []string{"steward"}, // but no Instance
	})
	publish(t, "--topic", "x", "--as", "steward", "--priority", "interrupt", "urgent")

	rec := &recorder{}
	if _, err := testSidecar(t, rec).Once(); err != nil {
		t.Fatal(err)
	}
	items, _ := ReadPending("dev-a")
	if len(items) != 1 || items[0].Delivery != DeliveryQueued {
		t.Fatalf("pending = %+v", items)
	}
	if !strings.Contains(items[0].Demoted, "instance") {
		t.Errorf("demotion reason = %q", items[0].Demoted)
	}
}

// A failed delivery must not lose the message either.
func TestFailedInterruptStillQueues(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{
		Subscriber: "dev-a", Topics: []string{"*"},
		Instance: "gone", InterruptFrom: []string{"steward"},
	})
	publish(t, "--topic", "x", "--as", "steward", "--priority", "interrupt", "urgent")

	rec := &recorder{fail: errTest}
	res, err := testSidecar(t, rec).Once()
	if err != nil {
		t.Fatalf("a failed interrupt broke the whole pass: %v", err)
	}
	if res.Demoted != 1 {
		t.Errorf("demoted = %d, want 1", res.Demoted)
	}
	items, _ := ReadPending("dev-a")
	if len(items) != 1 {
		t.Fatal("a failed interrupt lost the notification")
	}
	// And the RECORD must say so. The buffer entry once kept delivery=interrupt
	// for a delivery that failed — a record asserting success because nothing
	// wrote down the failure. An operator reading the buffer must be able to see
	// that the agent was never actually interrupted.
	if items[0].Delivery != DeliveryQueued {
		t.Errorf("delivery = %q, want queued — the interrupt failed", items[0].Delivery)
	}
	if !strings.Contains(items[0].Demoted, "interrupt failed") {
		t.Errorf("the failure is not recorded on the entry: %q", items[0].Demoted)
	}
}

var errTest = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "no such instance" }

// --- rate limiting --------------------------------------------------------

// Pollution is the biggest failure mode: steering quality collapses as messages
// multiply. The cap is deliberately low — and, again, the surplus is DEMOTED,
// not dropped.
func TestInterruptRateLimitDemotesTheSurplus(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{
		Subscriber: "dev-a", Topics: []string{"*"},
		Instance: "s", InterruptFrom: []string{"steward"}, MaxPerMin: 2,
	})
	for i := range 5 {
		publish(t, "--topic", "x", "--as", "steward", "--priority", "interrupt",
			"urgent", string(rune('a'+i)))
	}

	rec := &recorder{}
	res, err := testSidecar(t, rec).Once()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 2 {
		t.Errorf("delivered %d interrupts, want 2 (the cap)", len(rec.got))
	}
	if res.Demoted != 3 {
		t.Errorf("demoted = %d, want 3", res.Demoted)
	}
	// All five still reach the agent, three of them queued.
	items, _ := ReadPending("dev-a")
	if len(items) != 5 {
		t.Fatalf("rate limiting dropped notifications: %d of 5 survived", len(items))
	}
	var queued int
	for _, p := range items {
		if p.Delivery == DeliveryQueued {
			queued++
			if !strings.Contains(p.Demoted, "rate limit") {
				t.Errorf("demotion reason = %q", p.Demoted)
			}
		}
	}
	if queued != 3 {
		t.Errorf("queued = %d, want 3", queued)
	}
}

// --- the pending buffer ---------------------------------------------------

// The clear is bounded by what was actually read. Truncating wholesale would
// discard anything the sidecar appended between the agent's read and its clear —
// a notification the agent never learns existed.
func TestClearPendingKeepsWhatArrivedAfterTheRead(t *testing.T) {
	isolate(t)
	for i, body := range []string{"first", "second"} {
		if err := AppendPending("dev-a", Pending{Seq: int64(i + 1), Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	read, err := ReadPending("dev-a")
	if err != nil {
		t.Fatal(err)
	}
	// A third arrives after the agent read the first two.
	if err := AppendPending("dev-a", Pending{Seq: 3, Body: "third"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearPending("dev-a", read[len(read)-1].Seq); err != nil {
		t.Fatal(err)
	}

	left, err := ReadPending("dev-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Body != "third" {
		t.Fatalf("the clear discarded a notification that arrived mid-read: %+v", left)
	}
}

func TestFormatPendingMarksUrgent(t *testing.T) {
	out := FormatPending([]Pending{
		{Topic: "code.api.Foo", Principal: "alice", Body: "changed", Delivery: DeliveryQueued},
		{Topic: "fleet.priority", Principal: "steward", Body: "stop", Delivery: DeliveryInterrupt},
	})
	if !strings.Contains(out, "code.api.Foo") || !strings.Contains(out, "alice") {
		t.Errorf("block is missing the sender or topic:\n%s", out)
	}
	if !strings.Contains(out, "[urgent]") {
		t.Errorf("an interrupt-tier item is not marked urgent:\n%s", out)
	}
	if FormatPending(nil) != "" {
		t.Error("an empty buffer should render as nothing at all")
	}
}

// --- registry -------------------------------------------------------------

func TestSubscriptionRoundTrip(t *testing.T) {
	isolate(t)
	want := Subscription{
		Subscriber: "dev-a", Topics: []string{"code.api.*", "task.1.done"},
		Instance: "sess", InterruptFrom: []string{"steward"}, MaxPerMin: 5,
	}
	subscribe(t, want)

	got, err := LoadSubscription("dev-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Subscriber != want.Subscriber || len(got.Topics) != 2 || got.MaxPerMin != 5 {
		t.Errorf("got %+v, want %+v", got, want)
	}
	all, err := Subscriptions()
	if err != nil || len(all) != 1 {
		t.Fatalf("Subscriptions() = %+v, %v", all, err)
	}
	if err := RemoveSubscription("dev-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSubscription("dev-a"); err == nil {
		t.Error("a removed subscription still loads")
	}
}

// A subscriber name becomes a path segment and comes from a flag or the
// environment, so it must not escape the registry directory.
func TestSubscriberNameCannotEscapeTheRegistry(t *testing.T) {
	isolate(t)
	for _, evil := range []string{"../../escape", "..", "a/b"} {
		if err := SaveSubscription(Subscription{Subscriber: evil, Topics: []string{"*"}}); err != nil {
			t.Fatalf("SaveSubscription(%q): %v", evil, err)
		}
	}
	all, err := Subscriptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("nothing was written at all")
	}
}

// Every subcommand name and alias must resolve to the command that owns it.
//
// This is not hypothetical: `watch` carried the aliases "sub"/"subscribe", and
// cobra resolves an alias ahead of a sibling's real name — so `bus subscribe`
// silently ran `watch` and then rejected its flags. An alias that shadows a
// sibling is worse than a missing one, because the wrong command runs.
func TestSubcommandNamesAreNotShadowedByAliases(t *testing.T) {
	root := NewBusCmd()

	// Collect every name and alias, and refuse a duplicate outright.
	seen := map[string]string{} // token -> owning command
	for _, c := range root.Commands() {
		for _, token := range append([]string{c.Name()}, c.Aliases...) {
			if owner, dup := seen[token]; dup {
				t.Errorf("%q is claimed by both %q and %q", token, owner, c.Name())
			}
			seen[token] = c.Name()
		}
	}

	// And each real name resolves to itself through cobra's own lookup.
	for _, want := range []string{"publish", "watch", "subscribe", "unsubscribe", "subscriptions", "sidecar", "pending"} {
		got, _, err := root.Find([]string{want})
		if err != nil {
			t.Errorf("Find(%q): %v", want, err)
			continue
		}
		if got.Name() != want {
			t.Errorf("%q resolved to the %q command", want, got.Name())
		}
	}
}
