package room

import (
	"context"
	"os"
	"testing"
	"time"
)

func collect(t *testing.T, opt StreamOptions) []StreamItem {
	t.Helper()
	var got []StreamItem
	if err := Stream(context.Background(), opt, func(it StreamItem) error {
		got = append(got, it)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestStreamDomainEventsHaveContiguousSeq(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	Record(EventSprint, "mgr", "sprint:#418", "decision", "picked A")
	Record(EventHandoff, "a", "handoff:h1", "created", "brief")
	Record(EventTodo, "a", "todo:9ba6d26c", "done", "done: x")
	got := collect(t, StreamOptions{})
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3: %+v", len(got), got)
	}
	for i, it := range got {
		if it.Resync != nil || it.Event.Seq != int64(i+1) {
			t.Fatalf("item %d = %+v, want contiguous seq %d", i, it, i+1)
		}
	}
	if got[0].Event.Type != EventSprint || got[0].Event.Topic != "decision" || got[0].Event.Target != "sprint:#418" {
		t.Fatalf("sprint event shape: %+v", got[0].Event)
	}
	only := collect(t, StreamOptions{Match: func(e Event) bool { return e.Type == EventTodo }})
	if len(only) != 1 || only[0].Event.Seq != 3 {
		t.Fatalf("kind filter: %+v", only)
	}
}

func TestStreamResumeAfterSeqHasNoGap(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	for i := 0; i < 5; i++ {
		Record(EventNote, "a", "", "", "n")
	}
	got := collect(t, StreamOptions{After: 2, Explicit: true})
	if len(got) != 3 || got[0].Event.Seq != 3 || got[2].Event.Seq != 5 {
		t.Fatalf("resume after 2: %+v", got)
	}
}

func TestStreamResyncWhenSincePrecedesRetention(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	emitAt(t, Event{Type: EventNote, Body: "old1"}, time.Now().Add(-10*24*time.Hour))
	emitAt(t, Event{Type: EventNote, Body: "old2"}, time.Now().Add(-10*24*time.Hour))
	Record(EventNote, "a", "", "", "fresh")
	setCursor(t, "watcher", 3, time.Now())
	if n, err := RotateTimeline(time.Now()); err != nil || n != 2 {
		t.Fatalf("rotate = %d, %v; want 2", n, err)
	}
	got := collect(t, StreamOptions{After: 1, Explicit: true})
	if len(got) != 2 || got[0].Resync == nil || got[0].Resync.Reason != ResyncRetention || got[0].Resync.Seq != 2 || got[0].Resync.ArchivedThrough != 2 {
		t.Fatalf("want retention resync at 2 then seq 3, got %+v", got)
	}
	if got[1].Event.Seq != 3 {
		t.Fatalf("after resync: %+v", got[1])
	}
	// Not asking for a position is not a loss: no resync.
	if plain := collect(t, StreamOptions{}); len(plain) != 1 || plain[0].Resync != nil {
		t.Fatalf("plain read: %+v", plain)
	}
}

func TestStreamResyncWhenPositionIsPastHead(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	Record(EventNote, "a", "", "", "n")
	got := collect(t, StreamOptions{After: 99, Explicit: true})
	if len(got) != 1 || got[0].Resync == nil || got[0].Resync.Reason != ResyncRebase || got[0].Resync.Seq != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestStreamFollowResyncsWhenGenerationChanges(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	for i := 0; i < 3; i++ {
		Record(EventNote, "a", "", "", "a long enough body to make the file larger than its replacement")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	items := make(chan StreamItem, 16)
	go Stream(ctx, StreamOptions{Follow: true, Poll: 20 * time.Millisecond}, func(it StreamItem) error {
		items <- it
		return nil
	})
	time.Sleep(200 * time.Millisecond)
	path, _ := timelinePath()
	if err := os.WriteFile(path, []byte(`{"type":"note","ts":"2026-01-01T00:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case it := <-items:
		if it.Resync == nil || it.Resync.Reason != ResyncRebase || it.Resync.Seq != 1 {
			t.Fatalf("got %+v, want rebase resync at head 1", it)
		}
	case <-ctx.Done():
		t.Fatal("no resync after the stream was replaced")
	}
	Record(EventNote, "a", "", "", "after")
	select {
	case it := <-items:
		if it.Event.Seq != 2 {
			t.Fatalf("got %+v, want seq 2", it)
		}
	case <-ctx.Done():
		t.Fatal("event after rebase not delivered")
	}
}

func TestBriefKeepsUTF8(t *testing.T) {
	if got := Brief("héllo wörld", 2); got != "h..." {
		t.Fatalf("got %q", got)
	}
}
