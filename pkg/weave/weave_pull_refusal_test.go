package weave

import (
	"os"
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

	out, _ := runWeave(t, "pull", "169")

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

	pull, _ := runWeave(t, "pull", "169")
	if strings.Contains(pull, "nothing to merge") {
		t.Fatalf("list reports held work but pull reports emptiness — the two paths disagree.\nlist: %s\npull: %s", list, pull)
	}
}

// "nothing to merge" is CORRECT for a run that genuinely holds nothing, and it
// must survive the fix — a refusal path that shouts about commits it does not
// have is the same defect pointed the other way.
func TestPullStillSaysNothingToMergeWhenGenuinelyEmpty(t *testing.T) {
	setupPullRefusalFixture(t, 170, "failed", 0)

	out, code := runWeave(t, "pull", "170")
	if code != 0 {
		t.Fatalf("pull of an empty run exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "nothing to merge") {
		t.Fatalf("a run with 0 commits ahead must still get the plain message: %s", out)
	}
}

// The JSON surface carries the same facts, so an agent reading structured
// output cannot conclude "empty" either.
func TestPullRefusalJSONCarriesStateAndCount(t *testing.T) {
	setupPullRefusalFixture(t, 169, "killed", 2)

	out, _ := runWeave(t, "pull", "169", "--json")
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
