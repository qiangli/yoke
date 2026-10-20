package hostsrc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/watch"
)

var at0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// fakeBoard is a message board in memory. Every test here runs against it:
// the real board is append-only and public, so a test that posted to it could
// not take it back.
type fakeBoard struct {
	posts  []bus.Post
	cursor map[string]int64
	sent   []bus.Post
	err    error
}

func newFakeBoard() *fakeBoard { return &fakeBoard{cursor: map[string]int64{}} }

func (b *fakeBoard) Posts() ([]bus.Post, error) {
	if b.err != nil {
		return nil, b.err
	}
	return append([]bus.Post(nil), b.posts...), nil
}

func (b *fakeBoard) SeenSeq(reader string) int64 { return b.cursor[strings.ToLower(reader)] }

func (b *fakeBoard) Post(p bus.Post) error {
	if b.err != nil {
		return b.err
	}
	b.sent = append(b.sent, p)
	return nil
}

func (b *fakeBoard) add(p bus.Post) bus.Post {
	p.Seq = int64(len(b.posts)) + 1
	if p.ID == "" {
		p.ID = "post-" + p.At
	}
	b.posts = append(b.posts, p)
	return p
}

func (b *fakeBoard) read(reader string, seq int64) { b.cursor[strings.ToLower(reader)] = seq }

func ts(d time.Duration) string { return at0.Add(d).Format(time.RFC3339) }

func TestMBSourceBaselineThenPostsAndReceipts(t *testing.T) {
	board := newFakeBoard()
	board.add(bus.Post{ID: "old", From: "conductor", To: "worker", Body: "ancient history", At: ts(-3 * time.Hour)})
	src := NewMBSource(board)
	src.Backfill = time.Hour

	evs, err := src.Poll(context.Background(), at0)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("baseline replayed history: %+v", evs)
	}

	board.add(bus.Post{ID: "m1", From: "conductor", To: "worker", Topic: "sprint", Body: "status?\nsecond line", At: ts(time.Minute)})
	evs, err = src.Poll(context.Background(), at0.Add(time.Minute))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one post", evs)
	}
	ev := evs[0]
	if ev.Source != "mb" || ev.Kind != watch.KindAdded || ev.Locator != "mb:m1" || ev.Key != "m1" {
		t.Fatalf("event = %+v", ev)
	}
	for name, want := range map[string]string{
		"from": "conductor", "to": "worker", "topic": "sprint",
		"conv": "topic:sprint", "directed": "true", "seq": "2",
	} {
		if got := ev.Attr(name); got != want {
			t.Fatalf("attr %s = %q, want %q", name, got, want)
		}
	}
	if ev.Text != "status?" {
		t.Fatalf("text = %q, want the first line only", ev.Text)
	}

	// Nothing new, and the cursor has not moved: no event at all. A watcher
	// that re-reported the same post every minute is one an operator mutes.
	if evs, err = src.Poll(context.Background(), at0.Add(2*time.Minute)); err != nil || len(evs) != 0 {
		t.Fatalf("idle poll = (%+v, %v)", evs, err)
	}

	// The recipient's own cursor moves past it: that is the READ rung, and it is
	// a different fact from the post existing.
	board.read("worker", 2)
	evs, err = src.Poll(context.Background(), at0.Add(3*time.Minute))
	if err != nil || len(evs) != 1 {
		t.Fatalf("receipt poll = (%+v, %v)", evs, err)
	}
	if evs[0].Field != "read" || evs[0].Attr("post") != "m1" || evs[0].Before != "queued" || evs[0].After != "read" {
		t.Fatalf("receipt = %+v", evs[0])
	}
	if evs, err = src.Poll(context.Background(), at0.Add(4*time.Minute)); err != nil || len(evs) != 0 {
		t.Fatalf("receipt repeated: (%+v, %v)", evs, err)
	}
}

func TestMBSourceUntopicedConversationIsThePair(t *testing.T) {
	board := newFakeBoard()
	src := NewMBSource(board)
	board.add(bus.Post{ID: "m1", From: "Worker", To: "conductor", Body: "done", At: ts(0)})
	evs, err := src.Poll(context.Background(), at0)
	if err != nil || len(evs) != 1 {
		t.Fatalf("poll = (%+v, %v)", evs, err)
	}
	if got := evs[0].Attr("conv"); got != "pair:conductor|worker" {
		t.Fatalf("conv = %q, want the sorted pair", got)
	}
}

func TestMBSourceBoardErrorIsASourceError(t *testing.T) {
	board := newFakeBoard()
	board.err = context.DeadlineExceeded
	src := NewMBSource(board)
	if evs, err := src.Poll(context.Background(), at0); err == nil || len(evs) != 0 {
		t.Fatalf("poll = (%+v, %v), want an error and no events", evs, err)
	}
}

func fakeTimeline(events ...room.Event) Timeline {
	return func(n int) ([]room.Event, error) {
		if n >= len(events) {
			return append([]room.Event(nil), events...), nil
		}
		return append([]room.Event(nil), events[len(events)-n:]...), nil
	}
}

func sprintEvent(seq int64, d time.Duration, target, kind, actor, body string) room.Event {
	return room.Event{
		Seq: seq, TS: at0.Add(d).Format(time.RFC3339), Type: room.EventSprint,
		Actor: actor, Target: target, Topic: kind, Body: body,
	}
}

func TestSprintSourceEmitsThreadEntries(t *testing.T) {
	events := []room.Event{
		sprintEvent(1, -2*time.Hour, "sprint:#480", "stage", "manager", "created"),
		{Seq: 2, TS: ts(0), Type: room.EventTodo, Target: "todo:1", Body: "unrelated"},
	}
	read := func(n int) ([]room.Event, error) { return events, nil }
	src := NewSprintSource(read)
	src.Backfill = time.Hour

	if evs, err := src.Poll(context.Background(), at0); err != nil || len(evs) != 0 {
		t.Fatalf("baseline = (%+v, %v), want nothing older than the backfill", evs, err)
	}

	events = append(events,
		sprintEvent(3, time.Minute, "sprint:#480", "stage", "manager", "started"),
		sprintEvent(4, 2*time.Minute, "sprint:#472", "progress", "worker", "checkpoint"),
	)
	evs, err := src.Poll(context.Background(), at0.Add(3*time.Minute))
	if err != nil || len(evs) != 2 {
		t.Fatalf("poll = (%+v, %v)", evs, err)
	}
	if evs[0].Locator != "sprint:480" || evs[0].Attr("kind") != "stage" || evs[0].Attr("author") != "manager" {
		t.Fatalf("event = %+v", evs[0])
	}
	if !strings.Contains(evs[0].Text, "started") {
		t.Fatalf("text = %q", evs[0].Text)
	}
	if evs[1].Locator != "sprint:472" {
		t.Fatalf("event = %+v", evs[1])
	}
	if evs, err := src.Poll(context.Background(), at0.Add(4*time.Minute)); err != nil || len(evs) != 0 {
		t.Fatalf("entries replayed: (%+v, %v)", evs, err)
	}
}

func TestSprintSourceNarrowsToOneCard(t *testing.T) {
	src := NewSprintSource(fakeTimeline(
		sprintEvent(1, 0, "sprint:#480", "stage", "manager", "started"),
		sprintEvent(2, 0, "sprint:#472", "stage", "manager", "started"),
	))
	src.Backfill = time.Hour
	src.Sprint = "#480"
	evs, err := src.Poll(context.Background(), at0)
	if err != nil || len(evs) != 1 || evs[0].Locator != "sprint:480" {
		t.Fatalf("poll = (%+v, %v)", evs, err)
	}
}

func TestInboxSinkPostsThroughTheBoard(t *testing.T) {
	board := newFakeBoard()
	sink := &InboxSink{BoardAPI: board, From: "watcher", To: []string{"conductor", "operator"}, Topic: "watch"}
	alert := watch.Alert{
		At: at0, Rule: "sprint-idle", State: watch.StateFire, Reason: watch.ReasonIdle,
		Locator: "sprint:480", Message: "stalled — no activity for 45m0s",
	}
	if err := sink.Deliver(alert); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(board.sent) != 2 {
		t.Fatalf("sent = %+v, want one post per recipient", board.sent)
	}
	p := board.sent[0]
	if p.From != "watcher" || p.To != "conductor" || p.Topic != "watch" {
		t.Fatalf("post = %+v", p)
	}
	for _, want := range []string{"watch ALERT", "sprint-idle", "sprint:480", "45m0s"} {
		if !strings.Contains(p.Body, want) {
			t.Fatalf("body %q missing %q", p.Body, want)
		}
	}
	alert.State = watch.StateClear
	if err := sink.Deliver(alert); err != nil {
		t.Fatalf("deliver clear: %v", err)
	}
	if !strings.Contains(board.sent[2].Body, "watch CLEAR") {
		t.Fatalf("clear body = %q", board.sent[2].Body)
	}
}

func TestInboxSinkRefusesAnUnattributableOrUnaddressedPost(t *testing.T) {
	board := newFakeBoard()
	if err := (&InboxSink{BoardAPI: board, To: []string{"conductor"}}).Deliver(watch.Alert{Rule: "r"}); err == nil {
		t.Fatal("a post with no sender must be refused, not sent anonymously")
	}
	if err := (&InboxSink{BoardAPI: board, From: "watcher"}).Deliver(watch.Alert{Rule: "r"}); err == nil {
		t.Fatal("a post with no recipient must be refused")
	}
	if len(board.sent) != 0 {
		t.Fatalf("sent = %+v, want nothing", board.sent)
	}
}
