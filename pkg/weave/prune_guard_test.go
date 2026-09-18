package weave

import (
	"os"
	"strings"
	"testing"
)

// `weave prune` must not decide what is expendable from a STATE LABEL.
//
// An agent branch lives ONLY inside its workspace clone until `weave pull`
// fetches it. So deleting the workspace does not leave its commits "unmerged" —
// it DESTROYS them. The branch half of prune has always known this (`git branch
// -d`, never -D). The workspace half did not, and the workspace is where the work
// actually is.
//
// Found with two live runs in the queue, both marked `failed`:
//   - #3 had crashed on exit with a complete, tested feature COMMITTED on its
//     branch. Those commits existed nowhere else on the machine.
//   - #4 had that feature's tests passing in an UNCOMMITTED tree.
//
// prune classified both by their state label and would have erased the lot,
// announcing only "will clean up 3 terminal item(s)".
func TestPruneHoldReasonNamesWhatWouldBeLost(t *testing.T) {
	// Unmerged commits: the dangerous case. Must point at salvage.
	got := weavePruneHoldReason(1, 0, 0)
	if !strings.Contains(got, "unmerged commit") {
		t.Errorf("reason must say the commits are unmerged: %q", got)
	}
	if !strings.Contains(got, "salvage") {
		t.Errorf("reason must point at the recovery path, or the user's only option looks like --force: %q", got)
	}

	// Uncommitted files: also work, also gone forever if deleted.
	got = weavePruneHoldReason(0, 3, 1)
	if !strings.Contains(got, "4 uncommitted file") {
		t.Errorf("reason must count dirty + untracked: %q", got)
	}
	if !strings.Contains(got, "salvage") || !strings.Contains(got, "preserve") {
		t.Errorf("reason must name a recoverable exit for uncommitted work: %q", got)
	}

	// Both.
	got = weavePruneHoldReason(2, 1, 0)
	if !strings.Contains(got, "unmerged commit") || !strings.Contains(got, "uncommitted file") {
		t.Errorf("reason must report both kinds of loss: %q", got)
	}

	// Every reason must name the escape hatch — a "skipped" with no way forward
	// is how people learn to reach for --force by reflex.
	for _, r := range []string{
		weavePruneHoldReason(1, 0, 0),
		weavePruneHoldReason(0, 1, 0),
	} {
		if !strings.Contains(r, "--force") {
			t.Errorf("reason must name --force: %q", r)
		}
	}
}

// A skip with no reason is a mystery, and a mystery teaches people to --force.
func TestPruneHoldReasonIsNeverEmpty(t *testing.T) {
	if got := weavePruneHoldReason(0, 0, 0); strings.TrimSpace(got) == "" {
		t.Error("hold reason must never be empty")
	}
}

// --force is an override of the retention decision, not permission to erase
// the only copy. A dirty terminal tree is committed, imported into the live
// repository, and only then is its workspace removed.
func TestWeavePruneForcePreservesDirtyTreeBeforeRemoval(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	workspace := dir + "/workspaces/issue-1"
	if err := os.MkdirAll(dir+"/workspaces", 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "clone", "-q", root, workspace)
	gitT(t, workspace, "checkout", "-qb", "agent/weave-issue-1")
	gitT(t, workspace, "commit", "--allow-empty", "-qm", "agent work")
	if err := os.WriteFile(workspace+"/uncommitted.txt", []byte("recover me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, Title: "dirty terminal work", State: "failed", Workspace: workspace,
		Branch: "agent/weave-issue-1", CommitsAhead: 1,
	}}}); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "prune", "--yes", "--force", "--json")
	if code != 0 || !strings.Contains(out, "refs/salvage/abandoned-1") {
		t.Fatalf("forced prune did not report preservation: exit=%d out=%s", code, out)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("preserved workspace was not removed: %v", err)
	}
	if got := gitT(t, root, "show", "refs/salvage/abandoned-1:uncommitted.txt"); got != "recover me" {
		t.Fatalf("salvage ref lost the dirty tree: %q", got)
	}
}
