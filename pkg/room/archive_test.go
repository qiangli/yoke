package room

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// disableAutoRotation stops Emit's opportunistic rotation from running
// during a test's own Emit calls — see pkg/bus's identical helper for why: a
// fresh room's throttle marker does not exist yet, so the very first emit
// always attempts a scan, before a test has finished seeding cursors.
func disableAutoRotation(t *testing.T) {
	t.Helper()
	touchRotationMarker()
}

// emitAt appends an event stamped at a specific time, bypassing Emit's
// now-if-empty default so a test can construct events already outside the
// retention window.
func emitAt(t *testing.T, e Event, at time.Time) {
	t.Helper()
	e.TS = at.UTC().Format(time.RFC3339)
	if err := Emit(e); err != nil {
		t.Fatalf("emit: %v", err)
	}
}

// setCursor writes a bus-style drain cursor directly (pkg/room cannot import
// pkg/bus — see archive.go's doc), backdating its mtime to stand in for "last
// successful drain at this time".
func setCursor(t *testing.T, subscriber string, seq int64, at time.Time) {
	t.Helper()
	dir := filepath.Join(Dir(), "cursors")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir cursors: %v", err)
	}
	p := filepath.Join(dir, cursorFileName(subscriber))
	if err := os.WriteFile(p, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o600); err != nil {
		t.Fatalf("write cursor: %v", err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatalf("chtimes cursor: %v", err)
	}
}

func TestRotateTimeline_ArchivesOldEventEveryoneHasDrained(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	emitAt(t, Event{Type: EventNote, Body: "old note"}, time.Now().Add(-10*24*time.Hour))

	events, err := Timeline(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("timeline = %+v (err %v), want 1 event", events, err)
	}
	seq := events[0].Seq

	setCursor(t, "watcher", seq, time.Now())

	n, err := RotateTimeline(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived %d events, want 1", n)
	}

	live, err := Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("live timeline still has %d events after archiving", len(live))
	}

	entries, err := os.ReadDir(archiveDir())
	if err != nil {
		t.Fatalf("reading archive dir: %v", err)
	}
	var found bool
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			b, rerr := os.ReadFile(filepath.Join(archiveDir(), e.Name()))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if strings.Contains(string(b), "old note") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("archived event not found in any monthly archive file")
	}
}

func TestRotateTimeline_NeverArchivesAnUndrainedDirectedEvent(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	emitAt(t, Event{Type: EventNotify, To: "away-worker", Principal: "sender", Body: "review this"},
		time.Now().Add(-30*24*time.Hour))

	// away-worker has never drained at all — no cursor file exists.
	n, err := RotateTimeline(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 0 {
		t.Fatalf("archived %d events, want 0 — an undrained directed event must never be swept", n)
	}
}

// TestRotateTimeline_AwaySubscriberStillProtectsTheirMail mirrors the
// board's identical guarantee: the directed event's own recipient must have
// drained past it no matter how long they have been away, even though a
// stale subscriber is normally excluded from blocking condition 2.
func TestRotateTimeline_AwaySubscriberStillProtectsTheirMail(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	emitAt(t, Event{Type: EventNotify, To: "away-worker", Principal: "sender", Body: "review this"},
		time.Now().Add(-30*24*time.Hour))
	events, _ := Timeline(0)
	seq := events[0].Seq

	// Behind the event, and long stale.
	setCursor(t, "away-worker", 0, time.Now().Add(-20*24*time.Hour))

	if n, err := RotateTimeline(time.Now()); err != nil || n != 0 {
		t.Fatalf("archived %d events (err %v), want 0 while away-worker has not drained past it", n, err)
	}

	// Once drained, the obligation is discharged.
	setCursor(t, "away-worker", seq, time.Now().Add(-20*24*time.Hour))
	if n, err := RotateTimeline(time.Now()); err != nil || n != 1 {
		t.Fatalf("archived %d events (err %v), want 1 once away-worker has drained past it", n, err)
	}
}

func TestRotateTimeline_StaleSubscriberDoesNotBlockAnUndirectedEvent(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	emitAt(t, Event{Type: EventNote, Body: "fleet-wide note"}, time.Now().Add(-10*24*time.Hour))

	// Stale: has not drained in far longer than the window.
	setCursor(t, "long-gone", 0, time.Now().Add(-30*24*time.Hour))

	n, err := RotateTimeline(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived %d events, want 1 — a stale subscriber must not block cleanup forever", n)
	}
}

func TestRotateTimeline_ActiveSubscriberBehindBlocksArchival(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	emitAt(t, Event{Type: EventNote, Body: "fleet-wide note"}, time.Now().Add(-10*24*time.Hour))

	// Non-stale (polled just now), but has not drained past this event.
	setCursor(t, "still-behind", 0, time.Now())

	n, err := RotateTimeline(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 0 {
		t.Fatalf("archived %d events, want 0 — an active subscriber has not caught up yet", n)
	}
}

func TestRotateTimeline_RecentEventsStayLive(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	if err := Emit(Event{Type: EventNote, Body: "just now"}); err != nil {
		t.Fatal(err)
	}

	n, err := RotateTimeline(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 0 {
		t.Fatalf("archived %d events, want 0 — nothing is old enough yet", n)
	}
}

// TestRotateTimeline_SeqIsNeverRenumbered is the identity guarantee: seq is
// referenced elsewhere (cursors, receipts) and must survive a rotation that
// removes everything before it.
func TestRotateTimeline_SeqIsNeverRenumbered(t *testing.T) {
	isolate(t)
	disableAutoRotation(t)
	emitAt(t, Event{Type: EventNote, Body: "archived eventually"}, time.Now().Add(-10*24*time.Hour))
	if err := Emit(Event{Type: EventNote, Body: "stays live"}); err != nil {
		t.Fatal(err)
	}
	before, _ := Timeline(0)
	if len(before) != 2 || before[0].Seq != 1 || before[1].Seq != 2 {
		t.Fatalf("seqs before rotation = %+v, want 1,2", before)
	}

	setCursor(t, "watcher", before[0].Seq, time.Now())
	n, err := RotateTimeline(time.Now())
	if err != nil || n != 1 {
		t.Fatalf("archived %d events (err %v), want 1", n, err)
	}

	live, err := Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Seq != 2 {
		t.Fatalf("surviving event = %+v, want seq 2 unchanged", live)
	}

	if err := Emit(Event{Type: EventNote, Body: "emitted after rotation"}); err != nil {
		t.Fatal(err)
	}
	after, _ := Timeline(0)
	if len(after) != 2 || after[1].Seq != 3 {
		t.Fatalf("event emitted after rotation = %+v, want seq 3 (never reuse an archived seq)", after)
	}
}

func TestEmit_RotatesOpportunisticallyOnWrite(t *testing.T) {
	isolate(t)
	// No cursors at all: condition 2 holds vacuously, and this event is not
	// directed, so condition 3 does not apply. A fresh room's throttle
	// marker does not exist yet, so this very emit attempts a scan.
	emitAt(t, Event{Type: EventNote, Body: "old and drained by nobody"}, time.Now().Add(-10*24*time.Hour))

	live, err := Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("live timeline has %d events, want the write's own opportunistic rotation to have archived it", len(live))
	}
}
