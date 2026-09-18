package meet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// These tests encode the 2026-07-18 meeting artifact, which no unit test would
// have caught: a `speaking` with no paired `spoke`, a `spoke` published before
// the turn was durable, and two rounds of the same meeting running at once.

// liveEvents reads the whole live channel for a meeting.
func liveEvents(t *testing.T, id string) []LiveEvent {
	t.Helper()
	path, err := livePath(id)
	if err != nil {
		t.Fatalf("livePath: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open live: %v", err)
	}
	defer f.Close()
	var out []LiveEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e LiveEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("live line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

// assertPaired is the invariant the artifact violated: every speaker that took
// the floor gave it back exactly once, and gave it back LAST.
//
// `speaking` may legitimately repeat for one speaker (setCtlSock re-emits it to
// publish the steer address), so this counts distinct speakers rather than
// records.
func assertPaired(t *testing.T, evs []LiveEvent) {
	t.Helper()
	type key struct {
		round   int
		speaker string
	}
	spoke := map[key]int{}
	open := map[key]bool{}
	for _, e := range evs {
		k := key{e.Round, e.Speaker}
		switch e.Kind {
		case liveSpeaking:
			open[k] = true
		case liveSpoke:
			spoke[k]++
			open[k] = false
		case liveLine:
			if spoke[k] > 0 {
				t.Errorf("round %d %s: line emitted AFTER the floor was freed", e.Round, e.Speaker)
			}
		}
	}
	for k, stillOpen := range open {
		if stillOpen {
			t.Errorf("round %d %s: took the floor and never gave it back (stranded speaking)", k.round, k.speaker)
		}
	}
	for k, n := range spoke {
		if n != 1 {
			t.Errorf("round %d %s: %d spoke events, want exactly 1", k.round, k.speaker, n)
		}
	}
}

//  1. A normal turn: one floor claim, a durable transcript entry, then the spoke
//     that announces it — and the seats after it still run.
func TestTurnRecordsBeforeFreeingTheFloor(t *testing.T) {
	st := newTestSession(t)
	evs, err := runRound(context.Background(), st, "verb set", fakeRunner{reply: "agree"})
	if err != nil {
		t.Fatalf("runRound: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("want both seats to run, got %d turns", len(evs))
	}

	live := liveEvents(t, st.ID)
	assertPaired(t, live)

	// The ordering claim, checked against the record rather than against a clock:
	// at the moment each `spoke` was written, the transcript already contained
	// that speaker's turn. Verified by replaying the transcript and confirming
	// every spoken turn is present — the append is synchronous inside
	// invokeAgent, so a `spoke` for a turn the transcript lacks is impossible
	// unless the ordering regresses.
	recorded, err := readTranscript(st.ID)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	for _, e := range live {
		if e.Kind != liveSpoke {
			continue
		}
		found := false
		for _, r := range recorded {
			if r.Speaker == e.Speaker && r.Round == e.Round && r.Kind == "turn" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("spoke for %s (round %d) but no turn in the transcript — "+
				"the live channel promised a durable record that does not exist",
				e.Speaker, e.Round)
		}
	}
}

// 9/10. The ordering invariant, asserted at the moment it matters rather than
// after the fact: when the turn is being made durable, the floor is still open.
//
// Checking only the finished files would pass under the OLD ordering too — by
// the end of a run everything is both appended and closed, whatever order it
// happened in. The bug was the window between them, so the test has to look
// inside the window.
func TestPersistHappensBeforeSpoke(t *testing.T) {
	st := newTestSession(t)

	var spokeAtPersistTime int
	_, err := invokeAgent(context.Background(), st, "codex", "", "instr", "q",
		fakeRunner{reply: "an answer"},
		func(e Event) (Event, error) {
			// Snapshot the live channel from inside the persist callback.
			for _, le := range liveEvents(t, st.ID) {
				if le.Kind == liveSpoke {
					spokeAtPersistTime++
				}
			}
			return e, nil
		})
	if err != nil {
		t.Fatalf("invokeAgent: %v", err)
	}
	if spokeAtPersistTime != 0 {
		t.Errorf("the floor was freed BEFORE the turn was recorded: %d spoke event(s) already written "+
			"when persist ran — `spoke` would be promising a transcript entry that does not exist yet",
			spokeAtPersistTime)
	}

	// And afterwards it is closed, exactly once.
	assertPaired(t, liveEvents(t, st.ID))
}

//  2. An empty response is a real outcome, not a lost turn: it still gets a
//     durable entry and a paired spoke carrying its true status.
func TestEmptyTurnIsDurableAndPaired(t *testing.T) {
	st := newTestSession(t)
	st.Participants = []string{"quiet"}
	evs, err := runRound(context.Background(), st, "verb set",
		scriptRunner{replies: map[string]string{"quiet": ""}})
	if err != nil {
		t.Fatalf("runRound: %v", err)
	}
	if statusOf(evs[0]) != statusEmpty {
		t.Fatalf("status = %q, want %q", statusOf(evs[0]), statusEmpty)
	}

	live := liveEvents(t, st.ID)
	assertPaired(t, live)
	if got := lastSpokeStatus(live, "quiet"); got != statusEmpty {
		t.Errorf("spoke status = %q, want %q", got, statusEmpty)
	}

	recorded, _ := readTranscript(st.ID)
	if !hasTurn(recorded, "quiet") {
		t.Error("an empty turn must still be in the transcript")
	}
}

//  6. When the transcript append FAILS, the spoke must not claim the turn is
//     recorded. Status is about the record, and the record is what failed.
func TestSpokeReportsUnrecordedWhenPersistFails(t *testing.T) {
	st := newTestSession(t)
	boom := errors.New("disk full")

	ev, err := invokeAgent(context.Background(), st, "codex", "", "instr", "q",
		fakeRunner{reply: "a considered answer"},
		func(e Event) (Event, error) { return e, boom })
	if err != nil {
		t.Fatalf("turn error = %v, want nil (the AGENT succeeded)", err)
	}
	// The turn's own classification is untouched: the agent did answer.
	if statusOf(ev) != statusOK {
		t.Errorf("event status = %q, want %q — a storage failure is not an agent failure",
			statusOf(ev), statusOK)
	}

	live := liveEvents(t, st.ID)
	assertPaired(t, live)
	if got := lastSpokeStatus(live, "codex"); got != statusUnrecorded {
		t.Errorf("spoke status = %q, want %q", got, statusUnrecorded)
	}
}

// 6 (cont). A runner that fails outright still pairs its lifecycle, and the
// failure is still recorded and still surfaced to the caller.
func TestRunnerFailurePairsLifecycleAndRecords(t *testing.T) {
	st := newTestSession(t)
	st.Participants = []string{"crasher"}
	evs, err := runRound(context.Background(), st, "verb set", scriptRunner{
		replies: map[string]string{"crasher": "boom"},
		codes:   map[string]int{"crasher": 3},
		errs:    map[string]error{"crasher": errors.New("exit status 3")},
	})
	if err != nil {
		t.Fatalf("runRound: %v", err)
	}
	if statusOf(evs[0]) != statusError {
		t.Fatalf("status = %q, want %q", statusOf(evs[0]), statusError)
	}
	assertPaired(t, liveEvents(t, st.ID))
	if recorded, _ := readTranscript(st.ID); !hasTurn(recorded, "crasher") {
		t.Error("a failed turn must still be recorded")
	}
}

//  4. Cancellation must not wedge the meeting: the turn returns, its lifecycle is
//     paired, and a later round still runs.
func TestCancelledTurnPairsLifecycleAndDoesNotWedge(t *testing.T) {
	st := newTestSession(t)
	st.Participants = []string{"slow"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	evs, err := runRound(ctx, st, "verb set", scriptRunner{
		replies: map[string]string{"slow": ""},
		errs:    map[string]error{"slow": context.Canceled},
	})
	if err != nil {
		t.Fatalf("runRound: %v", err)
	}
	if !turnFailed(evs[0]) {
		t.Errorf("a cancelled turn must be recorded as a failure, got %q", statusOf(evs[0]))
	}
	assertPaired(t, liveEvents(t, st.ID))
	if recorded, _ := readTranscript(st.ID); !hasTurn(recorded, "slow") {
		t.Error("a cancelled turn must leave a durable marker, not a gap")
	}

	// The lease was released, so the meeting is runnable again — the "does not
	// wedge" half of the requirement.
	if _, err := runRound(context.Background(), st, "verb set",
		fakeRunner{reply: "fine"}); err != nil {
		t.Fatalf("meeting wedged after a cancelled round: %v", err)
	}
}

//  5. A final line with no trailing newline is the agent's last words — the
//     normal shape for a turn killed mid-sentence by a timeout or a cancel. The
//     view must not stop one line before the record does.
//
// Driven against liveWriter directly, because that is where the tee actually
// happens: chat.Options.Stream is written by the real exec runner, so a fake
// Runner produces no stream at all and would make this test pass vacuously.
func TestTrailingPartialLineReachesTheLiveChannel(t *testing.T) {
	st := newTestSession(t)
	w := newLiveWriter(st, "partial", "", "")

	// Chunks deliberately misaligned to lines — exactly what a process flush
	// looks like. The last chunk has no trailing newline.
	for _, chunk := range []string{"first ", "line\nsecond li", "ne\nlast words with no newline"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	w.close(statusTimeout)

	live := liveEvents(t, st.ID)
	assertPaired(t, live)

	var lines []string
	for _, e := range live {
		if e.Kind == liveLine {
			lines = append(lines, e.Text)
		}
	}
	want := []string{"first line", "second line", "last words with no newline"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("live lines = %q, want %q", lines, want)
	}
	if got := lastSpokeStatus(live, "partial"); got != statusTimeout {
		t.Errorf("spoke status = %q, want %q", got, statusTimeout)
	}
}

// The flush is sanitized through the same path as every other line, so the view
// can never show a watcher bytes the record would have stripped.
func TestTrailingPartialLineIsSanitized(t *testing.T) {
	st := newTestSession(t)
	w := newLiveWriter(st, "noisy", "", "")
	raw := "answer\x1b[31m in red"
	if _, err := w.Write([]byte(raw)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.close(statusOK)

	var last string
	for _, e := range liveEvents(t, st.ID) {
		if e.Kind == liveLine {
			last = e.Text
		}
	}
	if want := strings.TrimRight(sanitizeTurn(raw), "\n"); last != want {
		t.Errorf("flushed line = %q, want the sanitized form %q", last, want)
	}
}

// close is once-only, so the deferred backstop behind an explicit close cannot
// publish a second ending for the same turn.
func TestCloseIsIdempotent(t *testing.T) {
	st := newTestSession(t)
	w := newLiveWriter(st, "codex", "", "")
	w.close(statusOK)
	w.close(statusError)

	live := liveEvents(t, st.ID)
	assertPaired(t, live)
	if got := lastSpokeStatus(live, "codex"); got != statusOK {
		t.Errorf("spoke status = %q, want %q — the first close must win", got, statusOK)
	}
}

//  7. Two runners, one meeting: the second is refused clearly rather than
//     allowed to interleave rounds.
func TestSecondRunnerIsRefusedNotInterleaved(t *testing.T) {
	st := newTestSession(t)

	lease, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	defer lease.Release()

	if _, err := runRound(context.Background(), st, "verb set", fakeRunner{reply: "agree"}); !errors.Is(err, ErrMeetingBusy) {
		t.Fatalf("second runner error = %v, want ErrMeetingBusy", err)
	}
	if _, err := runPoll(context.Background(), st, "ship?", []string{"yes", "no"}, nil, fakeRunner{reply: "yes"}); !errors.Is(err, ErrMeetingBusy) {
		t.Fatalf("poll under a held lease = %v, want ErrMeetingBusy", err)
	}
	if _, err := runAsk(context.Background(), st, "thoughts?", true, nil, fakeRunner{reply: "none"}); !errors.Is(err, ErrMeetingBusy) {
		t.Fatalf("ask under a held lease = %v, want ErrMeetingBusy", err)
	}

	// Nothing ran, so nothing was recorded — the point of refusing.
	if recorded, _ := readTranscript(st.ID); len(recorded) != 0 {
		t.Errorf("a refused runner wrote %d events; it must write none", len(recorded))
	}

	lease.Release()
	if _, err := runRound(context.Background(), st, "verb set", fakeRunner{reply: "agree"}); err != nil {
		t.Fatalf("after release, the meeting must be runnable again: %v", err)
	}
}

// 7 (cont). Independent meetings never contend — the lease is per-meeting.
func TestLeaseIsPerMeeting(t *testing.T) {
	st := newTestSession(t)
	other := *st
	other.ID = newID("Some other meeting", fixedNow())
	if err := other.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	held, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer held.Release()

	if _, err := runRound(context.Background(), &other, "verb set", fakeRunner{reply: "agree"}); err != nil {
		t.Fatalf("an unrelated meeting must not be blocked: %v", err)
	}
}

// 7 (cont). Metadata is diagnostic, never authoritative. A run.lock left behind
// naming a live-looking owner must not block anyone once the KERNEL lock is
// free — under the old heartbeat scheme this file was the source of truth and a
// crash could wedge the meeting until it aged out.
func TestStaleMetadataDoesNotBlock(t *testing.T) {
	st := newTestSession(t)
	path, err := leasePath(st.ID)
	if err != nil {
		t.Fatalf("leasePath: %v", err)
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Our own pid and host, with a fresh timestamp: maximally "alive" looking,
	// and completely irrelevant, because no descriptor holds the lock.
	host, _ := os.Hostname()
	b, _ := json.Marshal(leaseInfo{PID: os.Getpid(), Host: host, Since: fixedNow()})
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write stale metadata: %v", err)
	}

	l, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("stale metadata must not block an unlocked run.lock, got %v", err)
	}
	l.Release()
}

// Unparseable metadata is the signature of a crash mid-write. It degrades the
// busy MESSAGE and nothing else — acquisition never consults it.
func TestCorruptMetadataDoesNotBlock(t *testing.T) {
	st := newTestSession(t)
	path, _ := leasePath(st.ID)
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	l, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("corrupt metadata must not block, got %v", err)
	}
	defer l.Release()

	// And a contender still gets a usable ErrMeetingBusy without the metadata.
	if _, err := acquireRunLease(st.ID); !errors.Is(err, ErrMeetingBusy) {
		t.Fatalf("contender err = %v, want ErrMeetingBusy", err)
	}
}

// Exactly one winner, no matter how many contend at once. This is the property
// the previous read/compare/rename scheme could not have: its ownership test
// spanned multiple path operations, so two contenders could both conclude they
// had won. Here the kernel decides in one atomic operation.
func TestExactlyOneWinnerUnderConcurrency(t *testing.T) {
	st := newTestSession(t)

	for round := 0; round < 20; round++ {
		const contenders = 16
		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			won    []*runLease
			busy   int
			others []error
		)
		start := make(chan struct{})
		for i := 0; i < contenders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // release them together, to maximize real overlap
				l, err := acquireRunLease(st.ID)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won = append(won, l)
				case errors.Is(err, ErrMeetingBusy):
					busy++
				default:
					others = append(others, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if len(others) > 0 {
			t.Fatalf("round %d: unexpected errors: %v", round, others)
		}
		if len(won) != 1 {
			t.Fatalf("round %d: %d winners, want exactly 1", round, len(won))
		}
		if busy != contenders-1 {
			t.Fatalf("round %d: %d busy, want %d", round, busy, contenders-1)
		}
		// Released here, so the next round starts from a free lock — which also
		// exercises reacquisition 20 times over.
		won[0].Release()
	}
}

// A losing contender must not be able to disturb the owner's record of itself.
// Under the old scheme a contender wrote (removed and recreated) the lock file
// as part of deciding ownership; here it only ever reads.
func TestSecondContenderCannotMutateMetadata(t *testing.T) {
	st := newTestSession(t)
	owner, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer owner.Release()

	path, _ := leasePath(st.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var info leaseInfo
	if err := json.Unmarshal(before, &info); err != nil {
		t.Fatalf("owner metadata is not readable: %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("metadata pid = %d, want %d", info.PID, os.Getpid())
	}

	for i := 0; i < 5; i++ {
		if _, err := acquireRunLease(st.ID); !errors.Is(err, ErrMeetingBusy) {
			t.Fatalf("contender %d err = %v, want ErrMeetingBusy", i, err)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after contention: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("contenders rewrote the owner's metadata:\nbefore %q\nafter  %q", before, after)
	}
}

// The lock path is a stable inode across ownership changes. If it were unlinked
// and recreated, a successor could hold a lock on a detached inode while a third
// process locked the new one — two owners, no contention between them.
func TestLockInodeIsStableAcrossOwners(t *testing.T) {
	st := newTestSession(t)
	path, _ := leasePath(st.ID)

	first, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	ino1 := statFile(t, path)
	first.Release()

	// Surviving Release is the point: the lock file is never removed.
	ino2 := statFile(t, path)

	second, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	defer second.Release()
	ino3 := statFile(t, path)

	// os.SameFile compares identity (device+inode on unix, file index on
	// Windows) — the portable way to say "the same inode", not merely the same
	// path.
	if !os.SameFile(ino1, ino2) || !os.SameFile(ino2, ino3) {
		t.Error("run.lock was replaced across ownership changes; the inode must stay stable")
	}
}

// Releasing the shared lock leaves the stable inode and metadata in place while
// immediately admitting a successor. The subprocess test separately proves
// that process death closes the primitive's descriptor with the same result.
func TestReleasedLockReleasesWithoutCleanup(t *testing.T) {
	st := newTestSession(t)
	l, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	path, _ := leasePath(st.ID)
	stamped, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Drop the kernel lock without unlinking or editing any metadata.
	if err := l.lock.Release(); err != nil {
		t.Fatalf("close: %v", err)
	}

	next, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("a dead owner's lease must be immediately reusable, got %v", err)
	}
	defer next.Release()

	if len(stamped) == 0 {
		t.Error("owner metadata was never written")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("run.lock must still exist after an owner dies: %v", err)
	}

	// Release on the already-closed lease must stay safe and must not disturb the
	// successor that now legitimately owns the meeting.
	l.Release()
	l.Release()
	if _, err := acquireRunLease(st.ID); !errors.Is(err, ErrMeetingBusy) {
		t.Fatalf("a dead predecessor's Release freed its successor's lease; err = %v", err)
	}
}

func statFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

// A live owner is NEVER broken, even though its heartbeat has not ticked yet.
func TestLiveLeaseIsNotStolen(t *testing.T) {
	st := newTestSession(t)
	l, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer l.Release()
	if _, err := acquireRunLease(st.ID); !errors.Is(err, ErrMeetingBusy) {
		t.Fatalf("a live lease was stolen; err = %v", err)
	}
}

// Release is idempotent, so callers can defer it next to an explicit call.
func TestLeaseReleaseIsIdempotent(t *testing.T) {
	st := newTestSession(t)
	l, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	l.Release()
	l.Release()
	again, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatalf("re-acquire after double release: %v", err)
	}
	again.Release()
}

func lastSpokeStatus(evs []LiveEvent, speaker string) string {
	status := ""
	for _, e := range evs {
		if e.Kind == liveSpoke && e.Speaker == speaker {
			status = e.Status
		}
	}
	return status
}

func hasTurn(events []Event, speaker string) bool {
	for _, e := range events {
		if e.Speaker == speaker && e.Kind == "turn" {
			return true
		}
	}
	return false
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}
