package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// TestSprintTickMutatesNothing is the load-bearing gate for this verb.
//
// The whole design rests on tick being a pure read — it must not refresh the
// lease (which would forge liveness for a manager that has stopped working) and
// must not consume a cursor (which would make a PEEK eat somebody's mail). Both
// are easy to reintroduce by calling a convenient helper that happens to write,
// and neither would show up in the output. So the assertion is on the BYTES of
// every store tick touches, not on any value tick reports.
func TestSprintTickMutatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", home)

	root := t.TempDir()
	sprintTestStory(t, root, 99401, "open work", "p0", todopkg.StatusTodo)

	s := &weaveStory{
		ID:         99401,
		Title:      "tick purity",
		StoryRoots: []string{root},
		Created:    time.Now().Add(-2 * time.Hour),
		Owner:      "tick-manager",
		Lease:      &weaveStoryLease{Holder: "tick-manager", At: time.Now()},
		Continuity: "where it stands",
		Thread: []weaveComment{
			{At: time.Now().Add(-90 * time.Minute), Author: "tick-manager", Kind: "progress", Body: "checkpoint"},
		},
	}

	// Mail the manager has not read. Its cursor must survive the tick.
	if err := bus.Publish(bus.Notification{Principal: "sender", To: "tick-manager", Body: "please review"}); err != nil {
		t.Skipf("bus unavailable in this environment: %v", err)
	}

	before := snapshotAgentState(t, home)

	first, err := collectSprintTick(t.TempDir(), 99401, "tick-manager")
	_ = first
	_ = err // the queue dir is empty here; the purity check below is what matters

	// Drive the real path: a queue dir holding this sprint.
	dir := t.TempDir()
	writeTickQueue(t, dir, s)
	beforeQueue := snapshotTree(t, dir)
	beforeState := snapshotAgentState(t, home)

	beforePending, beforeDirect := unreadCounts(t, "tick-manager")

	tick, err := collectSprintTick(dir, 99401, "tick-manager")
	if err != nil {
		t.Fatalf("collectSprintTick: %v", err)
	}
	afterPending, afterDirect := unreadCounts(t, "tick-manager")
	if tick.As != "tick-manager" {
		t.Fatalf("As = %q, want tick-manager", tick.As)
	}

	if got := snapshotTree(t, dir); got != beforeQueue {
		t.Errorf("tick wrote under the sprint queue dir — it must not refresh the lease, touch UpdatedAt, or persist a probe cache\nbefore:\n%s\nafter:\n%s", beforeQueue, got)
	}
	if after := snapshotAgentState(t, home); after != beforeState {
		t.Errorf("tick wrote under the agent state dir; before=%d entries after=%d\n%s\n---\n%s",
			strings.Count(beforeState, "\n"), strings.Count(after, "\n"), beforeState, after)
	}
	_ = before

	// The cursors specifically. Each bus view is compared against ITSELF before
	// and after, never against the other: a record can be represented in the
	// directed timeline without having been materialised into the pending
	// buffer, so "directed is 1 and pending is 0" is a normal steady state and
	// asserting across the two would fail for a reason that has nothing to do
	// with consumption.
	if afterPending != beforePending {
		t.Errorf("pending mail went %d -> %d across a tick — counting is a peek, not a read", beforePending, afterPending)
	}
	if afterDirect != beforeDirect {
		t.Errorf("directed mail went %d -> %d across a tick — tick advanced the notification cursor", beforeDirect, afterDirect)
	}
	if tick.Mail.Directed == 0 {
		t.Fatal("fixture published no reachable mail, so the cursor checks above proved nothing")
	}
}

func unreadCounts(t *testing.T, who string) (pending, direct int) {
	t.Helper()
	if p, err := bus.UnreadPending(who); err == nil {
		pending = len(p)
	}
	if d, _, err := bus.UnreadNotifications(who); err == nil {
		direct = len(d)
	}
	return pending, direct
}

// TestSprintTickBaselineIsTheLastActionNotTheLastLook pins the choice in the
// header: the delta is measured from when the manager last DID something.
//
// A baseline keyed on looking would let a manager poll in a loop and see an
// empty delta forever, because the delta would be empty precisely BECAUSE it
// kept looking. This asserts a system entry does not count as an action and
// another principal's entry does not either.
func TestSprintTickBaselineIsTheLastActionNotTheLastLook(t *testing.T) {
	act := time.Now().Add(-40 * time.Minute)
	s := &weaveStory{
		ID:      99402,
		Created: time.Now().Add(-6 * time.Hour),
		Thread: []weaveComment{
			{At: act, Author: "tick-manager", Kind: "decision", Body: "assigned #1"},
			{At: time.Now().Add(-10 * time.Minute), Author: "someone-else", Kind: "progress", Body: "checkpoint"},
			{At: time.Now().Add(-1 * time.Minute), Author: "tick-manager", Kind: "system", Body: "created in backlog"},
		},
	}
	since, why := sprintTickBaseline(s, "tick-manager")
	if !since.Equal(act) {
		t.Fatalf("baseline = %s (%s), want the manager's own last non-system entry %s", since, why, act)
	}
	if !strings.Contains(why, "decision") {
		t.Errorf("reason = %q, want it to name the entry kind", why)
	}

	// A manager that has recorded nothing falls back to the time-box, and says so.
	start := time.Now().Add(-3 * time.Hour)
	s.Boxes = []weaveStoryBox{{StartedAt: start}}
	since, why = sprintTickBaseline(s, "fresh-manager")
	if !since.Equal(start) {
		t.Fatalf("fresh baseline = %s, want the box start %s", since, start)
	}
	if !strings.Contains(why, "recorded nothing") {
		t.Errorf("reason = %q, want it to say the manager has recorded nothing", why)
	}
}

// TestSprintTickBoardDeltaCountsOnlyWhatItCanProve pins the Opened/Closed split.
//
// An issue record carries Created and Closed and no modified-at field, so a
// single "changed" count would have implied a re-title or re-priority was
// visible when it is not. Two counts name exactly what is provable.
func TestSprintTickBoardDeltaCountsOnlyWhatItCanProve(t *testing.T) {
	root := t.TempDir()
	s := &weaveStory{ID: 99403, StoryRoots: []string{root}}
	since := time.Now().Add(-time.Hour)

	old := sprintTestStory(t, root, s.ID, "pre-existing", "p1", todopkg.StatusTodo)
	st := todopkg.RepoStore(root)
	it, _ := todopkg.ResolveRef(st, old)
	it.Created = time.Now().Add(-3 * time.Hour)
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}

	sprintTestStory(t, root, s.ID, "brand new", "p0", todopkg.StatusTodo)
	sprintTestStory(t, root, s.ID, "blocked one", "p2", todopkg.StatusBlocked)

	b := sprintTickReadBoard(s, since)
	if b.Opened != 2 {
		t.Errorf("Opened = %d, want 2 (the two created after the baseline)", b.Opened)
	}
	if b.Open != 3 {
		t.Errorf("Open = %d, want 3", b.Open)
	}
	if b.Blocked != 1 {
		t.Errorf("Blocked = %d, want 1", b.Blocked)
	}
	if b.Unowned != 3 {
		t.Errorf("Unowned = %d, want 3 (none assigned)", b.Unowned)
	}
	if b.Closed != 0 {
		t.Errorf("Closed = %d, want 0", b.Closed)
	}
}

// TestSprintTickBriefWithNoCheckpointReadsStale pins the evidence rule: an
// undated brief is stale, never fresh. Absence of evidence is not success.
func TestSprintTickBriefWithNoCheckpointReadsStale(t *testing.T) {
	none := sprintTickReadBrief(&weaveStory{ID: 99404})
	if none.Present || !none.Stale {
		t.Errorf("missing brief = %#v, want present=false stale=true", none)
	}

	undated := sprintTickReadBrief(&weaveStory{ID: 99404, Continuity: "text with no checkpoint entry behind it"})
	if !undated.Present || !undated.Stale {
		t.Errorf("undated brief = %#v, want present=true stale=true", undated)
	}

	fresh := sprintTickReadBrief(&weaveStory{
		ID:         99404,
		Continuity: "recent",
		Thread:     []weaveComment{{At: time.Now().Add(-5 * time.Minute), Kind: "progress", Body: "checkpoint"}},
	})
	if fresh.Stale {
		t.Errorf("5-minute-old brief = %#v, want stale=false", fresh)
	}
}

// TestSprintTickRendersTheSeatHintOnlyWhenItIsActionable keeps the worksheet
// quiet in the normal case: a manager holding its own seat does not need to be
// told how to take it.
func TestSprintTickRendersTheSeatHintOnlyWhenItIsActionable(t *testing.T) {
	held := renderTickToString(sprintTick{Sprint: 7, As: "m", Seat: sprintTickSeat{Holder: "m", State: "held"}})
	if strings.Contains(held, "seat:") {
		t.Errorf("held seat printed a hint:\n%s", held)
	}
	free := renderTickToString(sprintTick{Sprint: 7, As: "m", Seat: sprintTickSeat{State: "free"}})
	if !strings.Contains(free, "sprint start 7 --owner") {
		t.Errorf("free seat did not name the take command:\n%s", free)
	}
	stale := renderTickToString(sprintTick{Sprint: 7, As: "m", Seat: sprintTickSeat{Holder: "ghost", State: "stale"}})
	if !strings.Contains(stale, "ghost stopped beating") {
		t.Errorf("stale seat did not name the lapsed holder:\n%s", stale)
	}
}

// TestSprintTickFleetSaysWhatItsEvidenceIsWorth pins prohibition 3: the tick
// never probes, so it must never let "installed" read as "signed in".
func TestSprintTickFleetSaysWhatItsEvidenceIsWorth(t *testing.T) {
	f := sprintTickReadFleet(t.TempDir())
	if !strings.Contains(f.Evidence, "NOT signed in") {
		t.Errorf("evidence = %q, want it to disclaim sign-in", f.Evidence)
	}
	if f.Total == 0 {
		t.Error("fleet reported no roster at all")
	}
}

func renderTickToString(t sprintTick) string {
	var sb strings.Builder
	renderSprintTick(&sb, t)
	return sb.String()
}

func writeTickQueue(t *testing.T, dir string, s *weaveStory) {
	t.Helper()
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	q.Stories = append(q.Stories, s)
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
}

// snapshotTree renders every file under root with its size and mtime, so any
// write at all — a rewritten queue, a persisted probe cache — shows as a diff.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var sb strings.Builder
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		fmt.Fprintf(&sb, "%s %d %s\n", path, info.Size(), info.ModTime().Format(time.RFC3339Nano))
		return nil
	})
	return sb.String()
}

// snapshotAgentState renders every file under the agent state root with its
// size and modification time, so any write at all shows up as a diff.
func snapshotAgentState(t *testing.T, home string) string {
	t.Helper()
	var sb strings.Builder
	for _, base := range []string{filepath.Join(home, ".bashy"), filepath.Join(home, ".agents")} {
		_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			sb.WriteString(path)
			sb.WriteString(" ")
			sb.WriteString(info.ModTime().Format(time.RFC3339Nano))
			sb.WriteString(" ")
			sb.WriteString(time.Duration(info.Size()).String())
			sb.WriteString("\n")
			return nil
		})
	}
	return sb.String()
}

// TestSprintTickWaitReturnsOnTheFirstChange pins --wait's contract: the
// duration is a CEILING, and a change returns early.
//
// It is a unit test rather than a live probe on purpose. The first attempt to
// prove this against a running host published its trigger through a command
// that refused for lack of a claimed identity, in a backgrounded subshell whose
// stderr was discarded — so the wait ran its full ceiling and the run LOOKED
// like a failure to detect the change when nothing had ever been sent. A gate
// whose trigger can silently not fire measures nothing.
func TestSprintTickWaitReturnsOnTheFirstChange(t *testing.T) {
	old := sprintTickPoll
	sprintTickPoll = 20 * time.Millisecond
	t.Cleanup(func() { sprintTickPoll = old })

	// Repeat the dynamic add because the race was only exposed when one scan saw
	// an issue's initial write and another saw its sprint-stamped rewrite.
	for attempt := 0; attempt < 25; attempt++ {
		root := t.TempDir()
		dir := t.TempDir()
		s := &weaveStory{ID: 99405, Title: "wait", StoryRoots: []string{root}, Created: time.Now().Add(-time.Hour)}
		writeTickQueue(t, dir, s)

		base, err := collectSprintTick(dir, 99405, "waiter")
		if err != nil {
			t.Fatal(err)
		}
		if base.Board.Open != 0 {
			t.Fatalf("attempt %d: fixture started with %d open stories, want 0", attempt, base.Board.Open)
		}

		done := make(chan sprintTick, 1)
		go func() { done <- waitForSprintChange(dir, 99405, "waiter", base, 30*time.Second) }()

		// Give the waiter one poll of quiet, then move the board.
		time.Sleep(60 * time.Millisecond)
		sprintTestStory(t, root, 99405, "arrived mid-wait", "p0", todopkg.StatusTodo)

		select {
		case got := <-done:
			if got.Board.Open != 1 {
				t.Fatalf("attempt %d: returned with Open=%d, want 1 — it did not observe the new story", attempt, got.Board.Open)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: wait did not return early on a board change; the ceiling was 30s", attempt)
		}
	}
}

// TestSprintTickWaitHonoursTheCeiling is the other half: with nothing moving,
// the wait must end, not hang.
func TestSprintTickWaitHonoursTheCeiling(t *testing.T) {
	oldPoll := sprintTickPoll
	sprintTickPoll = 10 * time.Millisecond
	t.Cleanup(func() { sprintTickPoll = oldPoll })

	dir := t.TempDir()
	s := &weaveStory{ID: 99406, Title: "quiet", StoryRoots: []string{t.TempDir()}, Created: time.Now().Add(-time.Hour)}
	writeTickQueue(t, dir, s)
	base, err := collectSprintTick(dir, 99406, "waiter")
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	waitForSprintChange(dir, 99406, "waiter", base, 250*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("quiet wait took %s, want ~250ms (the ceiling)", elapsed)
	}
}

// TestSprintCheckpointAndCommentFileUnderTheHoldersName drives the REAL verbs.
//
// checkpoint and comment used to resolve their author BEFORE the sprint was
// loaded, through weaveConductorName — which consults only the ephemeral session
// identity and falls back to the literal string "conductor". A manager holding
// the seat under its own name therefore filed its checkpoints as "conductor",
// while `sprint goal evidence` filed under the real name: one actor, two names,
// on one sprint. checkpoint's own success line named the holder correctly at the
// same time, which is how it stayed unnoticed.
//
// The consequence is not cosmetic, which is why this test sits beside the tick:
// the tick measures "what changed since you last acted" from the manager's own
// thread entries, so an entry filed under a foreign name is invisible to the
// manager who wrote it — the baseline never advances and every tick re-reports
// the same delta. That was observed live on sprint 126.
//
// It runs the cobra commands rather than the helpers they call. An earlier
// version of this test asserted on weaveStoryConductorName directly and passed
// against the BROKEN code, because it reimplemented the fix instead of exercising
// it.
func TestSprintCheckpointAndCommentFileUnderTheHoldersName(t *testing.T) {
	const holder = "named-manager"
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// The ambient session identity is deliberately something ELSE, so a fix that
	// merely reads the environment cannot pass.
	t.Setenv("WEAVE_CONDUCTOR", "some-other-session")
	seedLiveAgent(t, holder)

	if out, code := runSprint(t, "add", "authorship"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", holder, "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "checkpoint", "1", "-m", "where it stands"); code != 0 {
		t.Fatalf("checkpoint exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "comment", "1", "-m", "a note"); code != 0 {
		t.Fatalf("comment exit=%d: %s", code, out)
	}

	out, code := runSprint(t, "show", "1")
	if code != 0 {
		t.Fatalf("show exit=%d: %s", code, out)
	}
	thread := out
	if i := strings.Index(out, "── thread ──"); i >= 0 {
		thread = out[i:]
	}
	for _, line := range strings.Split(thread, "\n") {
		if !strings.Contains(line, "(progress):") && !strings.Contains(line, "(note):") {
			continue
		}
		if strings.Contains(line, "conductor (") || strings.Contains(line, "some-other-session (") {
			t.Errorf("entry filed under a name its author does not hold:\n  %s\nfull thread:\n%s", strings.TrimSpace(line), thread)
		}
		if !strings.Contains(line, holder+" (") {
			t.Errorf("entry not attributed to the seat holder %q:\n  %s", holder, strings.TrimSpace(line))
		}
	}
}
