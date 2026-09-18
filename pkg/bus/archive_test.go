package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// old returns an RFC3339 timestamp well outside the default retention
// window, so a post built with it is immediately eligible on condition 1.
func old(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

// disableAutoRotation stops PostMessageSeq's opportunistic rotation from
// running during a test's own post() calls, by pre-touching the throttle
// marker — the same marker rotateBoardOpportunistic checks. Without this, a
// fresh temp board's very first post always trips an attempt (no marker
// exists yet), and that attempt can archive an old post before the test has
// finished setting up its readers. Tests below want RotateBoard as a unit,
// called explicitly, once, with a fully set up board.
func disableAutoRotation(t *testing.T) {
	t.Helper()
	touchRotationMarker()
}

func post(t *testing.T, p Post) int64 {
	t.Helper()
	seq, err := PostMessageSeq(p)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return seq
}

// touchReaderAt backdates a reader's seen-cursor mtime, standing in for "last
// polled at this time" — see boardReaderStates. It seeds a cursor at seq 0
// when the reader has none: MarkSeen(reader, 0) itself is a no-op against an
// absent cursor (0 <= SeenSeq's zero-value default), so the file has to be
// created directly here.
func touchReaderAt(t *testing.T, reader string, at time.Time) {
	t.Helper()
	p := seenPath(reader)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", reader, err)
		}
		if err := os.WriteFile(p, []byte("0\n"), 0o644); err != nil {
			t.Fatalf("seed cursor for %s: %v", reader, err)
		}
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", reader, err)
	}
}

// archiveMonthPathFor locates the (single) monthly archive file rotation has
// written, without duplicating archiveMonthKey's date math in the test.
func archiveMonthPathFor(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(archiveDir())
	if err != nil {
		t.Fatalf("reading archive dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			return filepath.Join(archiveDir(), e.Name())
		}
	}
	t.Fatal("no archive file found")
	return ""
}

func TestRotateBoard_ArchivesOldBroadcastEveryoneHasRead(t *testing.T) {
	boardInTempHome(t)
	disableAutoRotation(t)
	seq := post(t, Post{From: "sender", Body: "old news", At: old(10 * 24 * time.Hour)})

	// A reader who has both read it and polled recently — non-stale, cursor
	// past the post, so condition 2 is satisfied.
	if err := MarkSeen("reader", seq); err != nil {
		t.Fatal(err)
	}

	n, err := RotateBoard(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived %d posts, want 1", n)
	}

	live, err := Posts()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("live board still has %d posts after archiving", len(live))
	}

	b, err := os.ReadFile(archiveMonthPathFor(t))
	if err != nil {
		t.Fatalf("reading the archive: %v", err)
	}
	if !strings.Contains(string(b), "old news") {
		t.Fatalf("archive does not contain the archived post:\n%s", b)
	}
}

func TestRotateBoard_NeverArchivesAnUnreadDirectedPost(t *testing.T) {
	boardInTempHome(t)
	disableAutoRotation(t)
	post(t, Post{From: "sender", To: "away-worker", Body: "please review", At: old(30 * 24 * time.Hour)})

	// away-worker never reads it at all — no cursor exists for them.
	n, err := RotateBoard(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 0 {
		t.Fatalf("archived %d posts, want 0 — an unread directed post must never be swept", n)
	}
	live, err := Posts()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("live board has %d posts, want the directed post still there", len(live))
	}
}

// TestRotateBoard_AwayAddresseeStillProtectsTheirMail is the scenario named
// directly in the retention rule: a directed post's own recipient can be
// away far longer than the window and condition 3 must still hold even
// though condition 2 would have excluded them as stale.
func TestRotateBoard_AwayAddresseeStillProtectsTheirMail(t *testing.T) {
	boardInTempHome(t)
	disableAutoRotation(t)
	seq := post(t, Post{From: "sender", To: "away-worker", Body: "please review", At: old(30 * 24 * time.Hour)})

	// away-worker HAS a cursor, but it is behind the post and long stale —
	// away far longer than the retention window.
	touchReaderAt(t, "away-worker", time.Now().Add(-20*24*time.Hour))
	if SeenSeq("away-worker") >= seq {
		t.Fatal("test setup: away-worker must be behind the post")
	}

	if n, err := RotateBoard(time.Now()); err != nil || n != 0 {
		t.Fatalf("archived %d posts (err %v), want 0 while the addressee has not read it", n, err)
	}

	// Once away-worker reads it, the obligation is discharged and rotation
	// may proceed (their cursor is still "stale" by poll time, but condition
	// 3 only requires that they HAVE read it now, not that they read it
	// recently).
	if err := MarkSeen("away-worker", seq); err != nil {
		t.Fatal(err)
	}
	if n, err := RotateBoard(time.Now()); err != nil || n != 1 {
		t.Fatalf("archived %d posts (err %v), want 1 once the addressee has read it", n, err)
	}
}

func TestRotateBoard_StaleReaderDoesNotBlockABroadcast(t *testing.T) {
	boardInTempHome(t)
	disableAutoRotation(t)
	seq := post(t, Post{From: "sender", Body: "fleet-wide notice", At: old(10 * 24 * time.Hour)})

	// A reader who has not polled in far longer than the window: stale, and
	// per the rule, excluded from condition 2 even though their cursor never
	// caught up.
	touchReaderAt(t, "long-gone", time.Now().Add(-30*24*time.Hour))
	if SeenSeq("long-gone") >= seq {
		t.Fatal("test setup: long-gone must be behind the post")
	}

	n, err := RotateBoard(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived %d posts, want 1 — a stale reader must not block a broadcast forever", n)
	}
}

func TestRotateBoard_ActiveReaderBehindBlocksArchival(t *testing.T) {
	boardInTempHome(t)
	disableAutoRotation(t)
	post(t, Post{From: "sender", Body: "fleet-wide notice", At: old(10 * 24 * time.Hour)})

	// A reader who polled recently (non-stale) but has not caught up yet.
	touchReaderAt(t, "still-behind", time.Now())

	n, err := RotateBoard(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 0 {
		t.Fatalf("archived %d posts, want 0 — an active reader has not passed it yet", n)
	}
}

func TestRotateBoard_RecentPostsStayLive(t *testing.T) {
	boardInTempHome(t)
	disableAutoRotation(t)
	post(t, Post{From: "sender", Body: "just now"})

	n, err := RotateBoard(time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 0 {
		t.Fatalf("archived %d posts, want 0 — nothing is old enough yet", n)
	}
}

// TestRotateBoard_SeqIsNeverRenumbered is the identity guarantee the whole
// design exists to protect: archiving a prefix must not change the sequence
// number of anything that stays live, or of anything posted afterward.
func TestRotateBoard_SeqIsNeverRenumbered(t *testing.T) {
	boardInTempHome(t)
	disableAutoRotation(t)
	seq1 := post(t, Post{From: "sender", Body: "archived eventually", At: old(10 * 24 * time.Hour)})
	seq2 := post(t, Post{From: "sender", Body: "stays live"})
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2", seq1, seq2)
	}

	if err := MarkSeen("reader", seq1); err != nil {
		t.Fatal(err)
	}
	n, err := RotateBoard(time.Now())
	if err != nil || n != 1 {
		t.Fatalf("archived %d posts (err %v), want 1", n, err)
	}

	live, err := Posts()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Seq != seq2 {
		t.Fatalf("surviving post seq = %+v, want seq %d unchanged", live, seq2)
	}

	seq3 := post(t, Post{From: "sender", Body: "posted after rotation"})
	if seq3 != 3 {
		t.Fatalf("post after rotation got seq %d, want 3 (never reuse an archived seq)", seq3)
	}
}

// TestPostMessageSeq_RotatesOpportunisticallyOnWrite is the "no daemon" half
// of the design: rotation must happen from an ordinary write, with nobody
// ever calling RotateBoard directly.
func TestPostMessageSeq_RotatesOpportunisticallyOnWrite(t *testing.T) {
	boardInTempHome(t)
	// No readers at all: condition 2 holds vacuously (nobody to be behind),
	// and this post is not directed, so condition 3 does not apply either.
	// A fresh board's throttle marker does not exist yet, so the very next
	// write attempts a scan — this is the case that exercises it.
	post(t, Post{From: "sender", Body: "old and unread by anybody", At: old(10 * 24 * time.Hour)})

	live, err := Posts()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("live board has %d posts, want the write's own opportunistic rotation to have archived it", len(live))
	}
}

func TestRotateBoard_AudienceSnapshotRefreshesAndResolvesOnce(t *testing.T) {
	boardInTempHome(t)
	disableAutoRotation(t)
	now := time.Now()
	calls := 0
	names := []string{"reader"}
	oldSelect := FleetSelect
	FleetSelect = func(Audience) ([]string, error) { calls++; return names, nil }
	t.Cleanup(func() { FleetSelect = oldSelect })
	for range 7 {
		post(t, Post{From: "tester", Audience: &Audience{Role: "conductor"}, Body: "notice", At: old(10 * 24 * time.Hour)})
	}
	touchReaderAt(t, "reader", now)
	touchReaderAt(t, "outsider", now)
	if count, err := RotateBoard(now); err != nil || count != 0 {
		t.Fatalf("unread live manager: archived=%d error=%v", count, err)
	}
	if calls != 1 {
		t.Fatalf("first scan calls=%d, want 1", calls)
	}
	names = nil // The former manager's lease expires before the next scan.
	if count, err := RotateBoard(now); err != nil || count != 7 {
		t.Fatalf("expired manager: archived=%d error=%v", count, err)
	}
	if calls != 2 {
		t.Fatalf("two scans calls=%d, want 2 across all posts/readers", calls)
	}
}
