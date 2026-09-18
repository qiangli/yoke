package weave

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func resourceCleanupFixture(t *testing.T) (string, string, *weaveItem) {
	t.Helper()
	isolateResourceLifecycle(t)
	repo := t.TempDir()
	initMemoryTestRepo(t, repo)
	base := weaveTestGit(t, repo, "rev-parse", "HEAD")
	if e := os.WriteFile(filepath.Join(repo, "landed.txt"), []byte("valuable committed work\n"), 0600); e != nil {
		t.Fatal(e)
	}
	gitE2E(t, repo, "add", ".")
	gitE2E(t, repo, "commit", "-qm", "landed")
	head := weaveTestGit(t, repo, "rev-parse", "HEAD")
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspaces", "issue-1")
	gitE2E(t, repo, "clone", "-q", repo, workspace)
	it := &weaveItem{ID: 1, State: "done", Workspace: workspace, BaseSHA: base, Head: head, CommitsAhead: 1}
	if e := saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}}); e != nil {
		t.Fatal(e)
	}
	return dir, repo, it
}
func TestWeaveResourceCleanupCountsBytes(t *testing.T) {
	dir, repo, it := resourceCleanupFixture(t)
	expected, e := weaveArtifactBytes(it.Workspace)
	if e != nil || expected == 0 {
		t.Fatal(expected, e)
	}
	actions := weavePruneOwnedRun(dir, 1, repo)
	if len(actions) != 1 || !actions[0].Done || !actions[0].BytesComplete || actions[0].ExpectedBytes != expected || actions[0].ActualBytes == 0 {
		t.Fatalf("%+v", actions)
	}
	if _, e = os.Stat(it.Workspace); !os.IsNotExist(e) {
		t.Fatal("workspace remains", e)
	}
}
func TestWeaveResourceCleanupProtectsWork(t *testing.T) {
	for _, scenario := range []string{"dirty", "untracked", "active", "competitor", "locked", "symlink", "uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			dir, repo, it := resourceCleanupFixture(t)
			switch scenario {
			case "dirty":
				_ = os.WriteFile(filepath.Join(it.Workspace, "README.md"), []byte("uncommitted"), 0600)
			case "untracked":
				_ = os.WriteFile(filepath.Join(it.Workspace, "private.txt"), []byte("private"), 0600)
			case "uncertain":
				it.ResourceReservationID = "orphan"
				_ = saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}})
			case "active":
				it.WrapperPid = os.Getpid()
				_ = saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}})
			case "competitor":
				_ = saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it, {ID: 2, State: "working", Workspace: it.Workspace}}})
			case "locked":
				lock, e := weaveRunLifecycleLock(dir, 1)
				if e != nil {
					t.Fatal(e)
				}
				defer lock.Release()
			case "symlink":
				outside := t.TempDir()
				_ = os.WriteFile(filepath.Join(outside, "valuable"), []byte("keep"), 0600)
				it.LogPath = filepath.Join(dir, "linked")
				if e := os.Symlink(outside, it.LogPath); e != nil {
					t.Skip(e)
				}
				_ = saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}})
			}
			actions := weavePruneOwnedRun(dir, 1, repo)
			if scenario == "symlink" {
				if _, e := os.Lstat(it.LogPath); e != nil {
					t.Fatal("symlink target removed", actions)
				}
				return
			}
			if _, e := os.Stat(it.Workspace); e != nil {
				t.Fatalf("valuable workspace removed: %s %+v %v", scenario, actions, e)
			}
			for _, a := range actions {
				if a.Done {
					t.Fatalf("unsafe action %+v", a)
				}
			}
		})
	}
}
func TestWeaveResourceCleanupFreshEligibility(t *testing.T) {
	q := &weaveQueue{Items: []*weaveItem{{ID: 1, State: "done", Workspace: "one"}}}
	if _, e := weaveCleanupEligible(q, 1, "one"); e != nil {
		t.Fatal(e)
	}
	q.Items[0].State = "working"
	if _, e := weaveCleanupEligible(q, 1, "one"); e == nil {
		t.Fatal("stale eligibility accepted")
	}
}

func TestWeaveResourceCleanupRejectsRecycledSprintLink(t *testing.T) {
	dir, repo, it := resourceCleanupFixture(t)
	actions := weavePruneOwnedRun(dir, it.ID, repo, time.Now().Add(-time.Hour))
	if len(actions) != 1 || actions[0].Done || actions[0].Err == "" {
		t.Fatalf("recycled slot accepted: %+v", actions)
	}
	if _, e := os.Stat(it.Workspace); e != nil {
		t.Fatal(e)
	}
}

func TestWeaveResourceCleanupRejectsCleanCommitAfterClaim(t *testing.T) {
	dir, repo, it := resourceCleanupFixture(t)
	claim := it.Workspace + ".reclaim-fixture"
	if e := os.Rename(it.Workspace, claim); e != nil {
		t.Fatal(e)
	}
	gitE2E(t, claim, "config", "user.email", "fixture@test.local")
	gitE2E(t, claim, "config", "user.name", "fixture")
	if e := os.WriteFile(filepath.Join(claim, "new-work"), []byte("new committed work"), 0600); e != nil {
		t.Fatal(e)
	}
	gitE2E(t, claim, "add", ".")
	gitE2E(t, claim, "commit", "-qm", "new work after claim")
	if e := weaveVerifyReclaimWorkspace(repo, "main", it, claim); e == nil {
		t.Fatal("clean unmerged commit after claim accepted")
	}
	if e := weaveConventionalArtifact(dir, 1, "log", filepath.Join(dir, "queue.json")); e == nil {
		t.Fatal("queue state accepted as a log")
	}
	if e := weaveConventionalArtifact(dir, 1, "workspace", filepath.Join(dir, "workspaces", "shared-dependency")); e == nil {
		t.Fatal("shared dependency accepted as run workspace")
	}
}
