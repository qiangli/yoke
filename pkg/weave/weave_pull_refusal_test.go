package weave

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE BUG, AS OBSERVED LIVE (2026-07-27).
//
// `weave pull 169` on a run an adversarial review had flipped to `failed`
// printed:
//
//	weave pull: nothing to merge
//
// for a run holding THREE commits of gate-passing work that existed nowhere
// else on the machine. The same weave, in the same session, printed the
// opposite fact one line later — "SALVAGEABLE: #169 ... hold committed work not
// merged to the base branch" — because `list` measures and pull did not.
//
// That is not a wording nit. "Nothing to merge" is a claim about the DIFF, and
// the code emitting it had never looked at the diff; it was saying "this run is
// not in a state I accept" in the words of "this run is empty". Someone who
// believes a run is empty reaches for `weave abandon` — the one verb that turns
// unmerged work into destroyed work.
//
// setupPullRefusalFixture builds a user repo plus a workspace clone carrying
// `commits` commits that the user repo cannot reach.
func setupPullRefusalFixture(t *testing.T, id int64, state string, commits int) (root, workspace string, dir string) {
	t.Helper()
	root = setupIsolationFixture(t)
	dir, _ = weaveQueueDir(root)
	// A real weave workspace lives under the queue dir's workspaces/ — the
	// containment `weave prune` checks before it removes anything.
	workspace = filepath.Join(dir, "workspaces", "issue-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, workspace, "clone", "-q", root, ".")
	baseSHA := gitT(t, workspace, "rev-parse", "HEAD")
	branch := "agent/weave-issue-1"
	gitT(t, workspace, "checkout", "-q", "-b", branch)
	for range commits {
		gitT(t, workspace, "commit", "--allow-empty", "-qm", "agent work")
	}
	t.Chdir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: id, State: state, Workspace: workspace, Branch: branch, BaseSHA: baseSHA,
		Title: "held work", CommitsAhead: commits,
	}}}); err != nil {
		t.Fatal(err)
	}
	return root, workspace, dir
}

// A non-pullable run that IS holding commits must not get an emptiness-shaped
// message. It must be told its real state, its real commit count, and the verb
// that would actually recover the work.
func TestPullRefusalNamesStateCommitsAndVerb(t *testing.T) {
	_, workspace, _ := setupPullRefusalFixture(t, 169, "failed", 3)

	out, code := runWeave(t, "pull", "169")
	if code == 0 {
		t.Fatalf("refused pull must exit non-zero: %s", out)
	}

	if strings.Contains(out, "nothing to merge") {
		t.Fatalf("pull claimed emptiness for a run holding 3 commits — the reported work-loss bug: %s", out)
	}
	for _, want := range []string{
		`"failed"`,    // the run's REAL state
		"3 commit",    // the count, measured
		"salvage 169", // the verb that WOULD work
		"NOT empty",   // said out loud, because the old message said the opposite
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pull refusal must contain %q: %s", want, out)
		}
	}
	// The refusal must not have destroyed or mutated anything.
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("a refusal must leave the workspace intact: %v", err)
	}
}

// The two code paths that answer "does this run still hold work" must agree.
// They disagreed in production: list said SALVAGEABLE, pull said nothing to
// merge, about the same run in the same second.
func TestPullAndListAgreeOnHeldWork(t *testing.T) {
	setupPullRefusalFixture(t, 169, "failed", 3)

	list, code := runWeave(t, "list")
	if code != 0 {
		t.Fatalf("weave list exit=%d: %s", code, list)
	}
	if !strings.Contains(list, "SALVAGEABLE") || !strings.Contains(list, "#169") {
		t.Fatalf("fixture is wrong: list does not report #169 as salvageable: %s", list)
	}

	pull, pullCode := runWeave(t, "pull", "169")
	if pullCode == 0 {
		t.Fatalf("pull that did not merge must exit non-zero: %s", pull)
	}
	if strings.Contains(pull, "nothing to merge") {
		t.Fatalf("list reports held work but pull reports emptiness — the two paths disagree.\nlist: %s\npull: %s", list, pull)
	}
}

// "nothing to merge" is CORRECT for a run that genuinely holds nothing, and it
// must survive the fix alongside the run's state and a non-zero exit — a pull
// that did not merge must say why and exit non-zero.
func TestPullStillSaysNothingToMergeWhenGenuinelyEmpty(t *testing.T) {
	setupPullRefusalFixture(t, 170, "failed", 0)

	out, code := runWeave(t, "pull", "170")
	if code == 0 {
		t.Fatalf("pull of a non-merged empty run must exit non-zero: %s", out)
	}
	if !strings.Contains(out, "nothing to merge") || !strings.Contains(out, `"failed"`) {
		t.Fatalf("a run with 0 commits ahead must name its state and say nothing to merge: %s", out)
	}
}

// The JSON surface carries the same facts, so an agent reading structured
// output cannot conclude "empty" or "ok" either.
func TestPullRefusalJSONCarriesStateAndCount(t *testing.T) {
	setupPullRefusalFixture(t, 169, "killed", 2)

	out, code := runWeave(t, "pull", "169", "--json")
	if code == 0 {
		t.Fatalf("JSON refusal must exit non-zero: %s", out)
	}
	if !strings.Contains(out, `"status": "error"`) {
		t.Fatalf("JSON envelope must mark error when nothing merged: %s", out)
	}
	if !strings.Contains(out, "not-pullable") {
		t.Fatalf("JSON result must classify the refusal, not omit it: %s", out)
	}
	if !strings.Contains(out, "2 commit") {
		t.Fatalf("JSON detail must name the commit count: %s", out)
	}
	if !strings.Contains(out, "killed") {
		t.Fatalf("JSON detail must name the run's real state: %s", out)
	}
}

func runWeaveSplitStreams(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := newWeaveCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err != nil {
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) {
			code = ec.ExitCode()
		} else {
			code = 1
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// Regression for 1ee0c4e5: `weave pull <N> --force` on an isolation-violated
// run that fails to merge (e.g. a silent failing suite gate or a merge
// conflict) must NOT exit 0 or leave stderr with only an isolation banner.
func TestPullForceNonMergeSaysWhyOnStderrAndExitsNonZero(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	if out, code := runWeave(t, "add", "force non-merge", "--suite-gate", "exit 1", "--json"); code != 0 {
		t.Fatalf("weave add exit=%d: %s", code, out)
	}
	script := `set -e
echo work > work.txt
git add work.txt
git -c user.email=a@a -c user.name=a commit -qm "agent commit"
printf 'unrelated\n' > ` + filepath.Join(root, "conductor-note.md")
	if out, code := runWeave(t, "start", "--run", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("weave start exit=%d: %s", code, out)
	}

	baseBefore := gitT(t, root, "rev-parse", "HEAD")
	stdout, stderr, code := runWeaveSplitStreams(t, "pull", "1", "--force")
	if code == 0 {
		t.Fatalf("weave pull --force that did not merge exited 0\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if strings.Contains(stderr, "merging isolation-violated") {
		t.Fatalf("stderr falsely announced merging before gates passed:\n%s", stderr)
	}
	for _, want := range []string{"suite-gate-failed", "suite gate exited 1"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr must explain why pull did not merge (missing %q):\n%s", want, stderr)
		}
	}
	if got := gitT(t, root, "rev-parse", "HEAD"); got != baseBefore {
		t.Fatalf("main moved despite failed suite gate: %s -> %s", baseBefore, got)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if it := findWeaveItem(q, 1); it == nil || it.State != "submitted" {
		t.Fatalf("run #1 should remain submitted after non-merge: %+v", it)
	}
}

// Regression for 1ee0c4e5: when an earlier fetch/attempt left the run branch
// ref in root and the conductor rebases the workspace branch onto an advanced
// main, `weave pull <N> --force` must merge the rebased branch cleanly rather
// than failing a non-fast-forward <branch>:<branch> fetch.
func TestPullForceMergesRebasedWorkspaceBranchAfterPriorFetch(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	if out, code := runWeave(t, "add", "rebased work", "--json"); code != 0 {
		t.Fatalf("weave add exit=%d: %s", code, out)
	}
	script := `set -e
echo v1 > seed.txt
git add seed.txt
git -c user.email=a@a -c user.name=a commit -qm "worker edit"
printf 'note\n' > ` + filepath.Join(root, "conductor-note.md")
	if out, code := runWeave(t, "start", "--run", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("weave start exit=%d: %s", code, out)
	}

	// Advance main on the same file so the pre-rebase branch conflicts, and
	// leave the pre-rebase branch ref in root (as an earlier fetch does).
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("main-advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "add", "seed.txt")
	gitT(t, root, "commit", "-qm", "advance main")

	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	gitT(t, root, "fetch", "-q", "--no-tags", it.Workspace, it.Branch+":"+it.Branch)

	// First pull --force must report the conflict, exit non-zero, and clean up
	// its private pull ref on failure.
	_, conflictErr, conflictCode := runWeaveSplitStreams(t, "pull", "1", "--force")
	if conflictCode == 0 || !strings.Contains(conflictErr, "conflict") {
		t.Fatalf("conflicting pull --force want non-zero conflict, got exit=%d stderr=%s", conflictCode, conflictErr)
	}
	if exec.Command(gitBin(), "-C", root, "show-ref", "--verify", "--quiet", "refs/weave/pull/1").Run() == nil {
		t.Fatal("refs/weave/pull/1 leaked after failed merge")
	}

	// Conductor rebases the workspace branch onto root's advanced main.
	gitT(t, it.Workspace, "fetch", "-q", root, "main")
	gitT(t, it.Workspace, "reset", "-q", "--hard", "FETCH_HEAD")
	if err := os.WriteFile(filepath.Join(it.Workspace, "seed.txt"), []byte("main-advance\nrebased-worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, it.Workspace, "add", "seed.txt")
	gitT(t, it.Workspace, "commit", "-qm", "rebased worker edit")

	stdout, stderr, code := runWeaveSplitStreams(t, "pull", "1", "--force")
	if code != 0 || !strings.Contains(stdout, "merged") {
		t.Fatalf("rebased pull --force failed (exit=%d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if exec.Command(gitBin(), "-C", root, "show-ref", "--verify", "--quiet", "refs/weave/pull/1").Run() == nil {
		t.Fatal("refs/weave/pull/1 leaked after successful merge")
	}
	got, err := os.ReadFile(filepath.Join(root, "seed.txt"))
	if err != nil || !strings.Contains(string(got), "rebased-worker") {
		t.Fatalf("rebased commit did not land on main: content=%q err=%v", got, err)
	}
}

func TestBarePullWithNothingQueuedExitsZeroAndNonZeroWhenQueuedRunFails(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	// 1. Bare pull with nothing queued is an idempotent no-op: exit 0 + stderr note.
	stdout, stderr, code := runWeaveSplitStreams(t, "pull")
	if code != 0 {
		t.Fatalf("bare pull with nothing queued want exit 0, got %d: stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "nothing to merge") {
		t.Fatalf("stderr must keep the nothing-to-merge note: %q", stderr)
	}
	jsonOut, _, jsonCode := runWeaveSplitStreams(t, "pull", "--json")
	if jsonCode != 0 || !strings.Contains(jsonOut, `"status": "ok"`) {
		t.Fatalf("bare pull --json with nothing queued want exit 0 ok, got %d: %s", jsonCode, jsonOut)
	}

	// 2. Once a queued run fails to merge (including killed_by refusal), bare pull
	// and explicit pull must say why on stderr and exit non-zero.
	dir, _ := weaveQueueDir(root)
	baseSHA := gitT(t, root, "rev-parse", "HEAD")
	if err := saveWeaveQueue(dir, &weaveQueue{NextID: 2, Root: root, Items: []*weaveItem{{
		ID: 1, Title: "killed run", State: "killed", Branch: "agent/weave-issue-1",
		BaseSHA: baseSHA, Head: baseSHA, CommitsAhead: 1, KilledBy: "watchdog",
	}}}); err != nil {
		t.Fatal(err)
	}
	_, bareErr, bareCode := runWeaveSplitStreams(t, "pull")
	if bareCode == 0 || !strings.Contains(bareErr, "killed") || !strings.Contains(bareErr, "watchdog") {
		t.Fatalf("bare pull with unmerged killed run want non-zero + reason on stderr, got exit=%d stderr=%q", bareCode, bareErr)
	}
	_, explicitErr, explicitCode := runWeaveSplitStreams(t, "pull", "1")
	if explicitCode == 0 || !strings.Contains(explicitErr, "killed") || !strings.Contains(explicitErr, "watchdog") {
		t.Fatalf("explicit pull 1 with unmerged killed run want non-zero + reason on stderr, got exit=%d stderr=%q", explicitCode, explicitErr)
	}
}
