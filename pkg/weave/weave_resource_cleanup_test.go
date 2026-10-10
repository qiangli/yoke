package weave

import (
	"os"
	"path/filepath"
	"runtime"
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
	// Windows keeps the stable lock sentinel rather than unlinking its open
	// handle. Do not count retained lock bytes as reclaimed.
	wantActions := 2
	if runtime.GOOS == "windows" {
		wantActions = 1
	}
	if len(actions) != wantActions || !actions[0].Done || !actions[0].BytesComplete || actions[0].ExpectedBytes != expected || actions[0].ActualBytes == 0 {
		t.Fatalf("%+v", actions)
	}
	if runtime.GOOS != "windows" && (actions[1].Kind != "lock" || !actions[1].Done) {
		t.Fatalf("lock not reclaimed after full success: %+v", actions[1])
	}
	// Cleanup must release its kernel ownership. The next owner can acquire
	// the same path, and a simultaneous contender must still be refused.
	lock, err := weaveRunLifecycleLock(dir, it.ID)
	if err != nil {
		t.Fatalf("cleanup kept lifecycle ownership: %v", err)
	}
	defer lock.Release()
	if contender, err := weaveRunLifecycleLock(dir, it.ID); err == nil {
		contender.Release()
		t.Fatal("cleanup broke lifecycle mutual exclusion")
	}
	q, e := loadWeaveQueue(dir)
	if e != nil {
		t.Fatal(e)
	}
	if got := q.Items[0]; got.Workspace != "" || got.Disposition != weaveDispositionMerged {
		t.Fatalf("row not compacted with its disposition: %+v", got)
	}
	if _, e = os.Stat(it.Workspace); !os.IsNotExist(e) {
		t.Fatal("workspace remains", e)
	}
}

func TestWeaveResourceCleanupClearsStaleCacheErrorOnlyOnSuccess(t *testing.T) {
	for _, scenario := range []string{"clean", "dirty"} {
		t.Run(scenario, func(t *testing.T) {
			dir, repo, it := resourceCleanupFixture(t)
			it.CleanupError = "managed GOCACHE: old unlinkat failure"
			if err := saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}}); err != nil {
				t.Fatal(err)
			}
			if scenario == "dirty" {
				if err := os.WriteFile(filepath.Join(it.Workspace, "private.txt"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			weavePruneOwnedRun(dir, it.ID, repo)
			q, err := loadWeaveQueue(dir)
			if err != nil {
				t.Fatal(err)
			}
			got := q.Items[0]
			if scenario == "clean" && got.CleanupError != "" {
				t.Fatalf("successful retry kept stale cleanup error: %q", got.CleanupError)
			}
			if scenario == "dirty" && got.CleanupError != it.CleanupError {
				t.Fatalf("failed retry cleared cleanup error: %q", got.CleanupError)
			}
		})
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

// sideBranchCommit parks a commit on a private side branch of the workspace
// and returns HEAD to the source-only branch — the manager's preserve-then-
// reset move that the measured S413 teardown lost.
func sideBranchCommit(t *testing.T, ws, branch string) string {
	t.Helper()
	gitE2E(t, ws, "config", "user.email", "fixture@test.local")
	gitE2E(t, ws, "config", "user.name", "fixture")
	gitE2E(t, ws, "checkout", "-q", "-b", branch)
	if e := os.WriteFile(filepath.Join(ws, "WEAVE_MEMORY.md"), []byte("generated only\n"), 0600); e != nil {
		t.Fatal(e)
	}
	gitE2E(t, ws, "add", ".")
	gitE2E(t, ws, "commit", "-qm", "generated metadata")
	sha := weaveTestGit(t, ws, "rev-parse", "HEAD")
	gitE2E(t, ws, "checkout", "-q", "-")
	return sha
}

func TestWeaveResourceCleanupKeepsUniqueSideBranch(t *testing.T) {
	dir, repo, it := resourceCleanupFixture(t)
	sha := sideBranchCommit(t, it.Workspace, "preserved/s413-run1-generated")
	if !weaveItemMerged(repo, "main", it) {
		t.Fatal("fixture: source HEAD should read merged")
	}
	if _, ok := weaveItemSettled(repo, "main", it); ok {
		t.Fatal("merged HEAD settled past a unique side branch")
	}
	actions := weavePruneOwnedRun(dir, it.ID, repo)
	for _, a := range actions {
		if a.Done && a.Kind != "lock" {
			t.Fatalf("unsafe action %+v", a)
		}
	}
	if got := weaveTestGit(t, it.Workspace, "rev-parse", "preserved/s413-run1-generated"); got != sha {
		t.Fatalf("side branch lost: %q", got)
	}
	if out := weaveTestGit(t, it.Workspace, "show", sha+":WEAVE_MEMORY.md"); out != "generated only" {
		t.Fatalf("side commit unreadable: %q", out)
	}
	q, e := loadWeaveQueue(dir)
	if e != nil || q.Items[0].Workspace == "" {
		t.Fatalf("row compacted while workspace kept: %+v %v", q.Items[0], e)
	}
}

func TestWeaveResourceCleanupKeepsUniqueTagAfterClaim(t *testing.T) {
	_, repo, it := resourceCleanupFixture(t)
	claim := it.Workspace + ".reclaim-fixture"
	if e := os.Rename(it.Workspace, claim); e != nil {
		t.Fatal(e)
	}
	if e := weaveVerifyReclaimWorkspace(repo, "main", it, claim); e != nil {
		t.Fatalf("clean claim refused: %v", e)
	}
	sha := sideBranchCommit(t, claim, "scratch")
	gitE2E(t, claim, "tag", "private-generated", sha)
	gitE2E(t, claim, "branch", "-D", "scratch")
	if e := weaveVerifyReclaimWorkspace(repo, "main", it, claim); e == nil {
		t.Fatal("quarantine-time check accepted a unique tag")
	}
}

func TestWeaveResourceCleanupCoveredRefsDoNotBlock(t *testing.T) {
	for _, scenario := range []string{"merged-branch", "upstream-tag", "salvaged"} {
		t.Run(scenario, func(t *testing.T) {
			dir, repo, it := resourceCleanupFixture(t)
			ws := it.Workspace
			switch scenario {
			case "merged-branch":
				gitE2E(t, ws, "branch", "old-feature", it.BaseSHA)
				gitE2E(t, ws, "tag", "local-base", it.BaseSHA)
			case "upstream-tag":
				// A tag the user repo holds off its base branch, fetched as-is.
				gitE2E(t, repo, "checkout", "-q", "-b", "release")
				_ = os.WriteFile(filepath.Join(repo, "rel.txt"), []byte("rel"), 0600)
				gitE2E(t, repo, "add", ".")
				gitE2E(t, repo, "commit", "-qm", "release")
				gitE2E(t, repo, "tag", "v1")
				gitE2E(t, repo, "checkout", "-q", "main")
				gitE2E(t, repo, "branch", "-D", "release")
				gitE2E(t, ws, "fetch", "-q", "origin", "refs/tags/v1:refs/tags/v1")
			case "salvaged":
				sha := sideBranchCommit(t, ws, "preserved/s413-run1-generated")
				gitE2E(t, repo, "fetch", "-q", ws, sha+":refs/weave/salvage/1")
				it.SalvageRef = "refs/weave/salvage/1"
				if e := saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}}); e != nil {
					t.Fatal(e)
				}
			}
			if refs := weaveUnsettledLocalRefs(repo, "main", it); len(refs) > 0 {
				t.Fatalf("covered refs blocked: %v", refs)
			}
			actions := weavePruneOwnedRun(dir, it.ID, repo)
			if len(actions) == 0 || !actions[0].Done {
				t.Fatalf("covered workspace not reclaimed: %+v", actions)
			}
			if _, e := os.Stat(ws); !os.IsNotExist(e) {
				t.Fatal("workspace remains", e)
			}
		})
	}
}

func TestWeaveResourceCleanupRemoteRefCannotHideUniqueLocalBranch(t *testing.T) {
	_, repo, it := resourceCleanupFixture(t)
	sha := sideBranchCommit(t, it.Workspace, "private-recovery")
	// Remote-tracking refs in a disposable clone are not a canonical backup.
	gitE2E(t, it.Workspace, "update-ref", "refs/remotes/cache/private", sha)
	if _, ok := weaveItemSettled(repo, "main", it); ok {
		t.Fatal("workspace remote ref concealed unique local branch")
	}
}
