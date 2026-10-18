package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sprint 467 story 2383: a terminal run whose delivery already landed upstream
// as a patch-equivalent commit and whose tip is preserved by an earlier
// abandon still held work OUTSIDE HEAD — a stash and a pre-squash side branch,
// the shapes found in the leftover workspaces. `abandon --force` preserved only
// HEAD, so the guarded teardown refused and abandon said "teardown deferred"
// without saying why. --force must preserve those commits too and then remove
// the clean workspace.
func setupPreviouslyAbandonedSideWork(t *testing.T, fastForward bool) (root, workspace, sha, stash, side string) {
	t.Helper()
	root, workspace, sha = setupAbandonGuardFixture(t)
	baseSHA := gitT(t, root, "rev-parse", "HEAD")
	gitT(t, root, "fetch", "-q", workspace, "agent/weave-issue-1:refs/salvage/abandoned-1")
	if fastForward {
		gitT(t, root, "merge", "-q", "--ff-only", "refs/salvage/abandoned-1")
	} else {
		gitT(t, root, "cherry-pick", "--allow-empty", "--keep-redundant-commits", sha)
	}
	gitT(t, workspace, "branch", "wip-pre-squash")
	gitT(t, workspace, "checkout", "-q", "wip-pre-squash")
	gitT(t, workspace, "commit", "--allow-empty", "-qm", "pre-squash only")
	side = gitT(t, workspace, "rev-parse", "HEAD")
	gitT(t, workspace, "checkout", "-q", "agent/weave-issue-1")
	if err := os.WriteFile(filepath.Join(workspace, "seed.txt"), []byte("stashed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, workspace, "stash", "-q")
	stash = gitT(t, workspace, "rev-parse", "refs/stash")
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "abandoned", Workspace: workspace, Branch: "agent/weave-issue-1",
		BaseSHA: baseSHA, Head: sha, CommitsAhead: 1,
		SalvageRef: "refs/salvage/abandoned-1", Disposition: weaveDispositionSuperseded,
		ResourceReservationID: "fixture-child", ResourceTerminated: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	return root, workspace, sha, stash, side
}

func TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork(t *testing.T) {
	root, workspace, sha, stash, side := setupPreviouslyAbandonedSideWork(t, false)
	t.Chdir(root)
	out, code := runWeave(t, "abandon", "1", "--force", "--disposition", "superseded", "--yes")
	t.Logf("abandon output:\n%s", out)
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	if strings.Contains(out, "left alone") {
		t.Fatalf("settled workspace left alone:\n%s", out)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace survived --force abandon: %v\n%s", err, out)
	}
	if got := gitT(t, root, "rev-parse", "refs/salvage/abandoned-1"); got != sha {
		t.Fatalf("salvage ref moved: %s want %s", got, sha)
	}
	for _, c := range []string{stash, side} {
		if refs := gitT(t, root, "for-each-ref", "--contains", c, "--format=%(refname)", "refs/salvage/"); refs == "" {
			t.Fatalf("commit %s destroyed without a salvage ref\n%s", c, out)
		}
	}
}

// Without --force (and without a disposition) a run whose HEAD landed but whose
// stash and side branch did not is not destroyed, and the operator is told
// exactly which refs hold the work.
func TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused(t *testing.T) {
	root, workspace, _, _, _ := setupPreviouslyAbandonedSideWork(t, true)
	t.Chdir(root)
	out, _ := runWeave(t, "abandon", "1", "--yes")
	t.Logf("abandon output:\n%s", out)
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("unforced abandon destroyed unpreserved side work: %v", err)
	}
	for _, want := range []string{"refs/stash", "refs/heads/wip-pre-squash"} {
		if !strings.Contains(out, want) {
			t.Fatalf("refusal does not name %s:\n%s", want, out)
		}
	}
}

// Live protection: an unverified child keeps the workspace, and abandon says so.
func TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused(t *testing.T) {
	root, workspace, _ := setupAbandonGuardFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, State: "killed", Workspace: workspace, Branch: "agent/weave-issue-1", CommitsAhead: 1,
		ResourceReservationID: "fixture-child",
	}}}); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "abandon", "1", "--force", "--yes")
	t.Logf("abandon output:\n%s", out)
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("workspace removed while child termination unverified: %v", err)
	}
	if !strings.Contains(out, "child termination remains unverified") {
		t.Fatalf("deferral does not say why:\n%s", out)
	}
}

// Collision/retry: two workspace refs that flatten to the same salvage name
// (heads/x/y and heads/x-y), plus a salvage ref left by an earlier attempt that
// already occupies a flattened name with a different tip (the side branch moved
// since). No existing salvage ref may move, and every original tip must stay
// reachable from a run-scoped salvage ref after the workspace is removed.
func TestWeaveAbandonForceSideRefCollisionNeverOverwritesSalvage(t *testing.T) {
	root, workspace, _, stash, side := setupPreviouslyAbandonedSideWork(t, false)
	tips := map[string]string{"stash": stash, "wip-pre-squash": side}
	for _, b := range []string{"x/y", "x-y"} {
		gitT(t, workspace, "checkout", "-q", "-b", b, "agent/weave-issue-1")
		gitT(t, workspace, "commit", "--allow-empty", "-qm", "side "+b)
		tips[b] = gitT(t, workspace, "rev-parse", "HEAD")
	}
	gitT(t, workspace, "checkout", "-q", "agent/weave-issue-1")
	// An earlier retry preserved an older wip-pre-squash tip under the plain name.
	old := gitT(t, root, "rev-parse", "HEAD~1")
	gitT(t, root, "update-ref", "refs/salvage/abandoned-1-heads-wip-pre-squash", old)
	before := gitT(t, root, "for-each-ref", "--format=%(refname) %(objectname)", "refs/salvage/")
	t.Chdir(root)
	out, code := runWeave(t, "abandon", "1", "--force", "--disposition", "superseded", "--yes")
	t.Logf("abandon output:\n%s", out)
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace survived --force abandon: %v\n%s", err, out)
	}
	for _, line := range strings.Split(before, "\n") {
		f := strings.Fields(line)
		if got := gitT(t, root, "rev-parse", f[0]); got != f[1] {
			t.Fatalf("existing salvage ref %s moved: %s -> %s", f[0], f[1], got)
		}
	}
	for name, c := range tips {
		if refs := gitT(t, root, "for-each-ref", "--contains", c, "--format=%(refname)", "refs/salvage/abandoned-1-*"); refs == "" {
			t.Fatalf("%s tip %s not reachable from a run-scoped salvage ref\n%s", name, c, out)
		}
	}
	// The colliding pair landed under distinct names, each at its exact tip.
	exact := 0
	for _, b := range []string{"x/y", "x-y"} {
		if refs := gitT(t, root, "for-each-ref", "--points-at", tips[b], "--format=%(refname)", "refs/salvage/abandoned-1-heads-x-y*"); refs != "" {
			exact++
		}
	}
	if n := gitT(t, root, "for-each-ref", "--format=%(refname)", "refs/salvage/abandoned-1-heads-x-y*"); exact != 2 {
		t.Fatalf("colliding refs not both preserved at exact tips: %d\n%s\n%s", exact, n, out)
	}
}
