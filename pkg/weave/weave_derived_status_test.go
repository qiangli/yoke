//go:build !windows

// The fixtures below drive real `sh` workers and assert on unix signal/errno
// names, like weave_terminal_preserve_unix_test.go; the code under test is
// cross-platform.

package weave

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// Sprint #314, stories #1240/#1241: a DERIVED STATUS asserted with more
// confidence than its evidence supports.
//
// Run #36 (observed live): the worker implemented, tested and COMMITTED
// bd94cf3 on its branch. The wrapper's terminal-time measurement was stale at
// 0, so weave recorded `no-op` — printing "0 commit(s) ahead ... @ bd94cf39",
// the very head it claimed did not exist — and then BOTH rescue paths refused
// the run (`pull` because it was not submitted, `salvage` because of the
// state word). Real, green, reviewed work was stranded behind a label.
//
// The rule these tests pin, from this package's own comments: a record
// written by a process that did not survive is not evidence of absence. If
// the artifact is on disk, ask the artifact.

// The classification itself must never conclude "no-op" while the branch
// measurably holds commits. The fixture reproduces the race deterministically:
// the verify command commits AFTER the wrapper's branch measurement but
// BEFORE the terminal state is classified — exactly the window run #36's
// commit landed in.
func TestRunWithCommitsIsNeverClassifiedNoOp(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	verify := `git -c user.email=a@a -c user.name=a commit -q --allow-empty -m "landed during measurement"`
	if out, code := runWeave(t, "add", "commit lands mid-measurement", "--verify", verify, "--json"); code != 0 {
		t.Fatalf("weave add exit=%d: %s", code, out)
	}
	if out, code := runWeave(t, "start", "--issue", "1", "--pty", "never", "--json", "--", "sh", "-c", "true"); code != 0 {
		t.Fatalf("weave start exit=%d: %s", code, out)
	}

	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil {
		t.Fatal("run #1 missing from queue")
	}
	if it.State == "no-op" {
		t.Fatalf("run holding a commit was classified %q — a run holding commits must NEVER be no-op: %#v", it.State, it)
	}
	if it.State != "submitted" || it.CommitsAhead != 1 {
		t.Fatalf("state=%q commits_ahead=%d, want submitted with the measured commit", it.State, it.CommitsAhead)
	}

	// The classifier itself, pinned directly: stale evidence says 0, the
	// workspace says 1 — the workspace wins, and the corrected count is
	// folded back into the evidence.
	ws := newWorkspaceWithCommit(t)
	ev := weaveTerminalEvidence{CommitsAhead: 0}
	if got := weaveTerminalStateMeasured(ws.dir, ws.base, 0, nil, "", &ev); got != "submitted" {
		t.Fatalf("measured classification = %q, want submitted for a branch holding a commit", got)
	}
	if ev.CommitsAhead != 1 || ev.Head == "" {
		t.Fatalf("corrected evidence not folded back: %+v", ev)
	}
	// And a genuinely empty run still reads no-op — the fix must not invert
	// the defect.
	empty := weaveTerminalEvidence{CommitsAhead: 0}
	if got := weaveTerminalStateMeasured(ws.dir, "HEAD", 0, nil, "", &empty); got != "no-op" {
		t.Fatalf("empty branch = %q, want no-op", got)
	}
}

// The displayed ahead-count must reflect the BRANCH, not the stale recorded
// field — run #36's status printed "0 commit(s) ahead ... @ <head>", naming
// the commit it denied.
func TestStatusReportsTheLiveAheadCount(t *testing.T) {
	_, _, dir := setupPullRefusalFixture(t, 36, "no-op", 1)
	// The #36 record: the wrapper's stale terminal measurement said zero.
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, 36)
		it.CommitsAhead = 0
		it.Head = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "status", "36", "--json")
	if code != 0 {
		t.Fatalf("weave status exit=%d: %s", code, out)
	}
	if strings.Contains(out, `"commits_ahead": 0`) {
		t.Fatalf("status echoed the stale recorded zero instead of measuring the branch: %s", out)
	}
	if !strings.Contains(out, `"commits_ahead": 1`) {
		t.Fatalf("status must report the live count from the workspace: %s", out)
	}

	text, code := runWeave(t, "status", "36")
	if code != 0 {
		t.Fatalf("weave status exit=%d: %s", code, text)
	}
	if !strings.Contains(text, "1 commit(s) ahead") {
		t.Fatalf("human status must print the measured count, got: %s", text)
	}
	if !strings.Contains(text, "SALVAGEABLE") {
		t.Fatalf("a stranded run must be flagged salvageable, got: %s", text)
	}
}

// The run #36 scenario end to end: a run recorded no-op/0 whose branch holds
// a commit is salvageable — the state word may not gate the rescue of work
// that measurably exists — and the salvage merges it.
func TestSalvageAcceptsCommittedWorkWhateverTheStateWord(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runWeave(t, "add", "the run #36 shape", "--json"); code != 0 {
		t.Fatalf("weave add exit=%d: %s", code, out)
	}
	script := `set -e
echo work > feature36.txt
git add feature36.txt
git -c user.email=a@a -c user.name=a commit -qm "the work weave said did not exist"`
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("weave start exit=%d: %s", code, out)
	}
	// Rewrite the record into what run #36's queue actually held: state
	// no-op, commits 0 — the stale wrapper measurement, contradicted by the
	// branch on disk.
	dir, _ := weaveQueueDir(root)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, 1)
		it.State = "no-op"
		it.CommitsAhead = 0
		it.Head = ""
		it.Disposition = weaveDispositionEmpty
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "salvage", "1", "--json")
	if code != 0 {
		t.Fatalf("salvage refused a run holding committed work because of its state word (exit %d): %s", code, out)
	}
	if !strings.Contains(out, `"status": "merged"`) {
		t.Fatalf("salvage did not merge the held work: %s", out)
	}
	if got := strings.TrimSpace(gitT(t, root, "log", "--format=%s", "-1")); got == "seed" {
		t.Fatalf("base branch unmoved after salvage: %s", got)
	}
	if got := gitT(t, root, "log", "--format=%s"); !strings.Contains(got, "the work weave said did not exist") {
		t.Fatalf("salvaged commit is not on the base branch: %s", got)
	}
}

// "tool exited with N" alone filed an ENOSPC death as a flaky test. The
// failure must carry its cause: the tail of the tool's captured stderr/PTY
// output alongside the exit status.
func TestToolFailureReportsTheCauseNotJustTheCode(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runWeave(t, "add", "dies of a full disk", "--json"); code != 0 {
		t.Fatalf("weave add exit=%d: %s", code, out)
	}
	script := `echo "sh: write error: No space left on device" >&2; exit 3`
	out, code := runWeave(t, "start", "--issue", "1", "--pty", "never", "--", "sh", "-c", script)
	if code == 0 {
		t.Fatalf("failing tool reported success: %s", out)
	}
	if !strings.Contains(out, "tool exited with 3") {
		t.Fatalf("the exit status must be kept: %s", out)
	}
	// The cause must be IN the failure report — not merely somewhere in the
	// passthrough scrollback. The joined form pins that.
	if !strings.Contains(out, "tool exited with 3 —") || !strings.Contains(out, "last output:") {
		t.Fatalf("failure reports only the code, not the cause: %s", out)
	}
	if !strings.Contains(out, "No space left on device") {
		t.Fatalf("the captured stderr tail must name the cause: %s", out)
	}

	// The error constructor, pinned directly. A known underlying error is
	// wrapped, not paraphrased: errors.Is must still see it.
	err := weaveToolFailureError(1, "", syscall.ENOSPC, "")
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("underlying error not wrapped: %v", err)
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("underlying error not named in the message: %v", err)
	}
	// A signalled child is reported as signalled, by name — not as an
	// inscrutable shifted code.
	err = weaveToolFailureError(143, "idle-timeout watchdog fired", nil, "")
	msg := err.Error()
	if !strings.Contains(msg, "killed by signal") || !strings.Contains(msg, "terminated") {
		t.Fatalf("signal death must be named: %v", err)
	}
	if !strings.Contains(msg, "exit 143") || !strings.Contains(msg, "idle-timeout watchdog fired") {
		t.Fatalf("exit code and kill reason must survive alongside the signal: %v", err)
	}
}
