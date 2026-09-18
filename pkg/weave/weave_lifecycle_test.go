package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE INVARIANT, as a test.
//
// Every weave item has a complete lifecycle — create, process, close — and no
// item may sit in a state that is not a determinate step toward closure. The
// state machine is declared in weave_lifecycle.go; these tests assert the
// declaration has no sinks and that the reaper actually fires the edges the
// declaration promises.

// No state is a sink. If an item can enter a state, the machine must offer it a
// way to done or abandoned — otherwise "it's stuck" is a permanent, structural
// property, not an incident.
func TestLifecycleHasNoLimbo(t *testing.T) {
	for _, state := range weaveLifecycleStates {
		if weaveIsClosedState(state) {
			continue
		}
		if got := weaveLifecycleTransitionsFrom(state); len(got) == 0 {
			t.Errorf("state %q has NO declared transition — it is a sink, i.e. a limbo", state)
		}
		if !weaveLifecycleReachesClosure(state) {
			t.Errorf("state %q cannot reach done/abandoned by any path — the lifecycle does not close from there", state)
		}
	}
}

// Closed means closed. A transition OUT of done/abandoned would mean the
// lifecycle never actually ends, and every "how many are left?" count becomes
// unreliable.
func TestClosedStatesAreFinal(t *testing.T) {
	for _, t2 := range weaveLifecycleTransitions {
		if weaveIsClosedState(t2.From) {
			t.Errorf("closed state %q has an outgoing transition to %q (%s) — closed must be final", t2.From, t2.To, t2.By)
		}
	}
}

// The table may only mention states the machine actually knows about; a typo
// would otherwise manufacture a phantom escape route and make the no-sink proof
// above vacuously pass.
func TestLifecycleTransitionsUseDeclaredStates(t *testing.T) {
	known := map[string]bool{}
	for _, s := range weaveLifecycleStates {
		known[s] = true
	}
	for _, tr := range weaveLifecycleTransitions {
		if !known[tr.From] {
			t.Errorf("transition from unknown state %q", tr.From)
		}
		if !known[tr.To] {
			t.Errorf("transition to unknown state %q", tr.To)
		}
		if strings.TrimSpace(tr.By) == "" {
			t.Errorf("transition %s -> %s does not say WHO fires it", tr.From, tr.To)
		}
	}
}

// Every non-closed state must either move on its own or be explicitly declared
// a steward decision. "Somebody will probably notice" is the third option this
// asserts does not exist.
func TestEveryOpenStateIsAutomaticOrAnAvowedStewardDecision(t *testing.T) {
	for _, state := range weaveLifecycleStates {
		if weaveIsClosedState(state) {
			continue
		}
		auto := false
		for _, tr := range weaveLifecycleTransitionsFrom(state) {
			if tr.Auto {
				auto = true
				break
			}
		}
		if auto {
			continue
		}
		if _, declared := weaveLifecycleNeedsSteward[state]; !declared {
			t.Errorf("state %q has no automatic transition and is not declared a steward decision — nothing is responsible for moving it", state)
		}
	}
}

// ---- the reaper actually fires those edges ---------------------------------

// THE GATE. Kill a wrapper out of band: the run must be reaped to
// failed(wrapper-died), NOT left "working" forever.
func TestReaperEndsWorkingWithDeadWrapper(t *testing.T) {
	pid := deadPID(t)
	q := &weaveQueue{Items: []*weaveItem{{
		ID: 1, Title: "dead wrapper", State: "working", WrapperPid: pid,
		StartedAt: time.Now().Add(-3 * time.Hour), CtlSock: "/tmp/x.sock",
	}}}

	actions := weaveReapPass(q, "", "", time.Now().UTC())

	it := q.Items[0]
	if it.State != "failed" {
		t.Fatalf("state = %q, want failed — a working run whose wrapper is dead has no process left that could ever terminalize it", it.State)
	}
	if !strings.Contains(it.Completion, "wrapper-died") {
		t.Errorf("completion = %q, must name wrapper-died as the cause", it.Completion)
	}
	if it.FinishedAt.IsZero() {
		t.Errorf("reaped item must carry a finish time")
	}
	if it.WrapperPid != 0 || it.CtlSock != "" {
		t.Errorf("dead wrapper's pid/socket must be cleared, got pid=%d sock=%q", it.WrapperPid, it.CtlSock)
	}
	if len(actions) != 1 || actions[0].To != "failed" || actions[0].From != "working" {
		t.Fatalf("actions = %+v, want one working->failed", actions)
	}
	// And it must never become success: a crash is a crash.
	if it.State == "submitted" || it.State == "done" {
		t.Errorf("reaper promoted a dead wrapper to %q — success may never be inferred from a process that did not survive", it.State)
	}
}

// A run weave never supervised (WrapperPid 0 — a manual/no-spawn session) has
// no death to detect. Terminalizing it would kill somebody's hand-driven work.
func TestReaperLeavesUnsupervisedWorkingAlone(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{ID: 1, State: "working", WrapperPid: 0}}}
	if actions := weaveReapPass(q, "", "", time.Now().UTC()); len(actions) != 0 {
		t.Fatalf("actions = %+v, want none", actions)
	}
	if q.Items[0].State != "working" {
		t.Errorf("state = %q, want working left untouched", q.Items[0].State)
	}
}

// A live wrapper is a live run. The reaper must not race a working agent.
func TestReaperLeavesLiveWrapperAlone(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{ID: 1, State: "working", WrapperPid: os.Getpid()}}}
	if actions := weaveReapPass(q, "", "", time.Now().UTC()); len(actions) != 0 {
		t.Fatalf("actions = %+v, want none for a live wrapper", actions)
	}
}

// An allocated orphan — workspace created, agent never launched — is swept.
func TestReaperSweepsOrphanAllocation(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{
		ID: 2, State: "allocated", WrapperPid: deadPID(t),
		LaunchPhase: "provisioning workspace",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}}}
	actions := weaveReapPass(q, "", "", time.Now().UTC())
	if q.Items[0].State != "failed" {
		t.Fatalf("state = %q, want failed", q.Items[0].State)
	}
	if len(actions) != 1 || actions[0].From != "allocated" {
		t.Fatalf("actions = %+v, want one allocated->failed", actions)
	}
}

// A no-spawn allocation (never started, no launcher) is a legitimate manual
// hold, not an orphan.
func TestReaperKeepsManualAllocation(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{ID: 3, State: "allocated"}}}
	if actions := weaveReapPass(q, "", "", time.Now().UTC()); len(actions) != 0 {
		t.Fatalf("actions = %+v, want none", actions)
	}
}

// submitted-with-no-merge past the threshold becomes a NAMED pending decision
// instead of a row that looks exactly like work in flight.
func TestReaperFlagsUnmergedSubmissionAsNeedsSteward(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{
		ID: 4, State: "submitted", CommitsAhead: 2,
		FinishedAt: time.Now().Add(-2 * time.Hour),
	}}}
	actions := weaveReapPass(q, "", "", time.Now().UTC())
	it := q.Items[0]
	if it.State != "submitted" {
		t.Fatalf("state = %q — the reaper must not merge around `weave pull`'s gates", it.State)
	}
	if !it.NeedsSteward || it.StewardReason == "" {
		t.Fatalf("needs_steward=%v reason=%q, want flagged with a reason", it.NeedsSteward, it.StewardReason)
	}
	if !strings.Contains(it.StewardReason, "weave pull") {
		t.Errorf("reason %q must name the decision that is owed", it.StewardReason)
	}
	if len(actions) != 1 || actions[0].Flag != "needs-steward" {
		t.Fatalf("actions = %+v, want one needs-steward flag", actions)
	}
}

// Fresh submissions are normal operation. A flag that fires during normal
// operation is a flag nobody reads.
func TestReaperLeavesFreshSubmissionAlone(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{
		ID: 5, State: "submitted", CommitsAhead: 1, FinishedAt: time.Now(),
	}}}
	if actions := weaveReapPass(q, "", "", time.Now().UTC()); len(actions) != 0 {
		t.Fatalf("actions = %+v, want none inside the threshold", actions)
	}
	if q.Items[0].NeedsSteward {
		t.Errorf("a fresh submission must not be flagged")
	}
}

// A stopped run sitting on committed work is flagged salvageable — MEASURED
// from the branch, because CommitsAhead is written by the process that died.
func TestReaperFlagsTerminalWithWorkAsSalvageable(t *testing.T) {
	ws := newWorkspaceWithCommit(t)
	q := &weaveQueue{Items: []*weaveItem{{
		ID: 6, State: "failed", Workspace: ws.dir, BaseSHA: ws.base, CommitsAhead: 0,
	}}}
	actions := weaveReapPass(q, "", "", time.Now().UTC())
	it := q.Items[0]
	if it.State != "failed" {
		t.Fatalf("state = %q — a crash is a crash; surfacing work is not asserting success", it.State)
	}
	if !it.Salvageable {
		t.Fatalf("failed run with a commit on the branch must be flagged salvageable (recorded commits_ahead was 0 — the dead wrapper never wrote it)")
	}
	if len(actions) != 1 || actions[0].Flag != "salvageable" {
		t.Fatalf("actions = %+v, want one salvageable flag", actions)
	}
	if !strings.Contains(strings.ToLower(actions[0].Reason), "do not re-run") {
		t.Errorf("reason %q must warn against re-running — that is the loss this exists to prevent", actions[0].Reason)
	}
}

// The reaper writes flags and state. It must never remove a workspace, a
// branch, or a commit — disposal stays an explicit guarded step.
func TestReaperNeverDestroysCommittedWork(t *testing.T) {
	ws := newWorkspaceWithCommit(t)
	head := lifecycleGitOut(t, ws.dir, "rev-parse", "HEAD")
	q := &weaveQueue{Items: []*weaveItem{
		{ID: 7, State: "working", WrapperPid: deadPID(t), Workspace: ws.dir, BaseSHA: ws.base},
	}}
	weaveReapPass(q, "", "", time.Now().UTC())

	if _, err := os.Stat(ws.dir); err != nil {
		t.Fatalf("reaper removed the workspace: %v", err)
	}
	if got := lifecycleGitOut(t, ws.dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved from %s to %s — the reaper must not touch commits", head, got)
	}
	if q.Items[0].Workspace != ws.dir {
		t.Errorf("reaper cleared the workspace pointer; the work would be unreachable")
	}
}

// Idempotence: a reaped queue is a fixed point. Re-running on every `weave
// list` must not re-report, re-comment, or churn the queue file.
func TestReaperIsIdempotent(t *testing.T) {
	ws := newWorkspaceWithCommit(t)
	now := time.Now().UTC()
	q := &weaveQueue{Items: []*weaveItem{
		{ID: 1, State: "working", WrapperPid: deadPID(t)},
		{ID: 2, State: "allocated", WrapperPid: deadPID(t), LaunchPhase: "provisioning workspace", StartedAt: now.Add(-time.Hour)},
		{ID: 3, State: "submitted", CommitsAhead: 1, FinishedAt: now.Add(-2 * time.Hour)},
		{ID: 4, State: "killed", Workspace: ws.dir, BaseSHA: ws.base},
	}}
	first := weaveReapPass(q, "", "", now)
	if len(first) != 4 {
		t.Fatalf("first pass reaped %d items, want 4: %+v", len(first), first)
	}
	comments := 0
	for _, it := range q.Items {
		comments += len(it.Comments)
	}

	second := weaveReapPass(q, "", "", now)
	if len(second) != 0 {
		t.Fatalf("second pass returned %+v, want none — the reaper must be a fixed point", second)
	}
	after := 0
	for _, it := range q.Items {
		after += len(it.Comments)
	}
	if after != comments {
		t.Errorf("second pass appended %d comments; an idempotent pass appends none", after-comments)
	}
}

// After a full reap, nothing is left in a state without a determinate next
// step — the invariant, end to end.
func TestAfterReapEveryOpenItemHasANextStep(t *testing.T) {
	now := time.Now().UTC()
	q := &weaveQueue{Items: []*weaveItem{
		{ID: 1, State: "working", WrapperPid: deadPID(t)},
		{ID: 2, State: "allocated", WrapperPid: deadPID(t), LaunchPhase: "provisioning workspace", StartedAt: now.Add(-time.Hour)},
		{ID: 3, State: "submitted", CommitsAhead: 1, FinishedAt: now.Add(-2 * time.Hour)},
		{ID: 4, State: "todo"},
		{ID: 5, State: "paused"},
	}}
	weaveReapPass(q, "", "", now)
	for _, it := range weaveLimboItems(q) {
		next := weaveNextSteps(it)
		if next == "" || strings.Contains(next, "NO DECLARED TRANSITION") {
			t.Errorf("#%d (%s) has no determinate next step: %q", it.ID, it.State, next)
		}
	}
}

// The locked entry point persists what the pass decided — a reap that only
// lived in memory would leave the limbo on disk for the next reader.
func TestReapQueuePersists(t *testing.T) {
	dir := t.TempDir()
	if err := saveWeaveQueue(dir, &weaveQueue{NextID: 2, Items: []*weaveItem{
		{ID: 1, State: "working", WrapperPid: deadPID(t), Created: time.Now()},
	}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	actions, err := weaveReapQueue(dir, "", "")
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want 1", actions)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if q.Items[0].State != "failed" {
		t.Fatalf("persisted state = %q, want failed", q.Items[0].State)
	}
}

// ---- helpers ---------------------------------------------------------------

// deadPID returns a PID that is certainly not alive: spawn a trivial process,
// wait for it, and reuse its number. Reaping is defined by "the wrapper is
// gone", so the test needs a genuinely gone process, not a guessed number.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("go", "version")
	if err := c.Start(); err != nil {
		t.Skipf("cannot spawn a probe process: %v", err)
	}
	_ = c.Wait()
	pid := c.Process.Pid
	if pidAlive(pid) {
		t.Skipf("pid %d still reported alive right after reaping it", pid)
	}
	return pid
}

type testWorkspace struct{ dir, base string }

// newWorkspaceWithCommit builds a real repo whose branch is one commit ahead of
// its base sha — the shape of a run that committed and then crashed.
func newWorkspaceWithCommit(t *testing.T) testWorkspace {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git("add", "-A")
	git("commit", "-qm", "base")
	base := lifecycleGitOut(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git("add", "-A")
	git("commit", "-qm", "the work that must not be lost")
	return testWorkspace{dir: dir, base: base}
}

func lifecycleGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Skipf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
