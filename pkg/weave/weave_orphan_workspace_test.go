package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// orphanTestQueueDir builds a queue directory with a workspaces/ subtree.
func orphanTestQueueDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "queue")
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func orphanTestClone(t *testing.T, queueDir, name string) string {
	t.Helper()
	path := filepath.Join(queueDir, "workspaces", name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.invalid"},
		{"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-qm", "base"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return path
}

// A directory no queue item points at is invisible to prune's item loop, which
// is what made 266 MB of sibling-dep mirror clones unreclaimable by any flag.
func TestOrphanWorkspaceTargetsFindsUnclaimedCleanClone(t *testing.T) {
	dir := orphanTestQueueDir(t)
	claimed := orphanTestClone(t, dir, "issue-1")
	orphan := orphanTestClone(t, dir, "coreutils")

	q := &weaveQueue{Items: []*weaveItem{{ID: 1, State: "done", Workspace: claimed}}}
	got, err := weaveOrphanWorkspaceTargets(dir, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly the unclaimed clone, got %d: %+v", len(got), got)
	}
	if got[0].Path != orphan {
		t.Fatalf("swept the wrong directory: %s", got[0].Path)
	}
	if got[0].Hold != "" {
		t.Fatalf("a clean unclaimed clone must be sweepable, held: %s", got[0].Hold)
	}
}

// Ownership is by PATH, so a claimed directory is protected whatever it is
// named — the issue-N convention is not what keeps work safe.
func TestOrphanWorkspaceTargetsClaimIsByPathNotName(t *testing.T) {
	dir := orphanTestQueueDir(t)
	oddly := orphanTestClone(t, dir, "not-an-issue-name")

	q := &weaveQueue{Items: []*weaveItem{{ID: 7, State: "working", Workspace: oddly}}}
	got, err := weaveOrphanWorkspaceTargets(dir, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a claimed directory must never be swept, got %+v", got)
	}
}

// The guard has to be stricter than the item path's: an orphan carries no
// BaseSHA, so there is no way to count what a delete would cost.
func TestOrphanWorkspaceHoldsWorkItCannotPriceExactly(t *testing.T) {
	t.Run("uncommitted tree", func(t *testing.T) {
		dir := orphanTestQueueDir(t)
		path := orphanTestClone(t, dir, "dirty")
		if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := weaveOrphanWorkspaceTargets(dir, &weaveQueue{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Hold == "" {
			t.Fatalf("a dirty orphan must be held, got %+v", got)
		}
	})

	t.Run("untracked file", func(t *testing.T) {
		dir := orphanTestQueueDir(t)
		path := orphanTestClone(t, dir, "untracked")
		if err := os.WriteFile(filepath.Join(path, "new.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := weaveOrphanWorkspaceTargets(dir, &weaveQueue{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Hold == "" {
			t.Fatalf("an orphan with untracked work must be held, got %+v", got)
		}
	})

	t.Run("agent branch exists only here", func(t *testing.T) {
		dir := orphanTestQueueDir(t)
		path := orphanTestClone(t, dir, "branchy")
		if out, err := exec.Command("git", "-C", path, "branch", "agent/weave-issue-9").CombinedOutput(); err != nil {
			t.Fatalf("git branch: %v: %s", err, out)
		}
		got, err := weaveOrphanWorkspaceTargets(dir, &weaveQueue{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Hold == "" {
			t.Fatalf("an orphan holding an agent branch must be held, got %+v", got)
		}
	})
}

// A directory that is not a repository at all has no branch to lose.
func TestOrphanWorkspaceNonRepoIsSweepable(t *testing.T) {
	dir := orphanTestQueueDir(t)
	path := filepath.Join(dir, "workspaces", "plain")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := weaveOrphanWorkspaceTargets(dir, &weaveQueue{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hold != "" {
		t.Fatalf("a non-repo orphan must be sweepable, got %+v", got)
	}
}

// A queue whose workspaces/ never existed must not error.
func TestOrphanWorkspaceTargetsMissingDirIsNotAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "queue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := weaveOrphanWorkspaceTargets(dir, &weaveQueue{})
	if err != nil {
		t.Fatalf("missing workspaces/ must be silent, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("nothing to sweep, got %+v", got)
	}
}

// Pin the command wiring, not only the discovery primitive: prune must remove
// an unclaimed clean directory and refuse one whose tree holds loose work.
func TestWeavePruneSweepsCleanOrphanAndKeepsDirtyOrphan(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	clean := orphanTestClone(t, dir, "clean-orphan")
	dirty := orphanTestClone(t, dir, "dirty-orphan")
	if err := os.WriteFile(filepath.Join(dirty, "loose.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "prune", "--yes", "--json")
	if code != 0 {
		t.Fatalf("prune exit=%d: %s", code, out)
	}
	if _, err := os.Stat(clean); !os.IsNotExist(err) {
		t.Fatalf("clean unclaimed workspace survived prune: %v", err)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatalf("dirty unclaimed workspace was not safely retained: %v", err)
	}
	if !strings.Contains(out, "orphaned-workspace") || !strings.Contains(out, "uncommitted file") {
		t.Fatalf("prune did not report the retained orphan and reason: %s", out)
	}
}

// Every advisory that offers to merge, inspect or diff a branch presupposes the
// workspace that holds it. Once pruned, the branch is gone with it.
func TestWorkspacePresentGatesAdvisories(t *testing.T) {
	dir := orphanTestQueueDir(t)
	live := orphanTestClone(t, dir, "issue-3")

	if !weaveWorkspacePresent(&weaveItem{ID: 3, Workspace: live}) {
		t.Fatal("a workspace on disk must read as present")
	}
	if weaveWorkspacePresent(&weaveItem{ID: 3, Workspace: filepath.Join(dir, "workspaces", "gone")}) {
		t.Fatal("a pruned workspace must not read as present")
	}
	if weaveWorkspacePresent(&weaveItem{ID: 3}) {
		t.Fatal("an item with no recorded workspace must not read as present")
	}
	if weaveWorkspacePresent(nil) {
		t.Fatal("nil must not read as present")
	}
}

// The forced-salvage commit must not read as the run's own conclusion.
func TestForcedSalvageCommitMessageSaysItWasNotGated(t *testing.T) {
	msg := weaveForcedSalvageCommitMessage(&weaveItem{ID: 4, Title: "some work"})
	for _, want := range []string{"NOT submitted", "--force", "work in progress"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message must say %q, got:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "some work") {
		t.Fatalf("message should carry the run title, got:\n%s", msg)
	}
}
