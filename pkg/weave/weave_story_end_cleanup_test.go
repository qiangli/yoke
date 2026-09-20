package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sprint 224 S4: `sprint end` refuses an undecided run, reclaims what the
// settled runs own, and closes only at residual 0 — without touching a queue
// another open sprint shares.

func endCleanupFixture(t *testing.T) (root, dir string) {
	t.Helper()
	unsetEnvForTest(t, weaveManagedGOCacheEnv)
	root = setupIsolationFixture(t)
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")
	t.Chdir(root)
	dir, _ = weaveQueueDir(root)
	// Two runs with unique work in conventional workspaces; #1 belongs to the
	// sprint under test, #2 to a second, still-open sprint on the same queue.
	for _, id := range []int64{1, 2} {
		if out, code := runWeave(t, "add", "run "+string(rune('0'+id)), "--points", "2"); code != 0 {
			t.Fatalf("weave add: %s", out)
		}
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range q.Items {
		ws := filepath.Join(dir, "workspaces", "issue-"+string(rune('0'+it.ID)))
		if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
			t.Fatal(err)
		}
		gitT(t, root, "clone", "-q", root, ws)
		gitT(t, ws, "checkout", "-qb", "agent/weave-issue-"+string(rune('0'+it.ID)))
		writeAndCommit(t, ws, "w.txt", "work\n", "agent work")
		it.State, it.Workspace, it.Branch, it.CommitsAhead = "submitted", ws, "agent/weave-issue-"+string(rune('0'+it.ID)), 1
		it.LaunchSpec = &weaveLaunchSpec{Tool: "true", MaxRuntime: 5 * time.Minute}
		cache := weaveManagedGOCachePath(nil, dir, it.ID)
		if err := os.MkdirAll(cache, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, "blob"), []byte("reproducible\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Base(root)
	for i, title := range []string{"under test", "neighbour"} {
		if out, code := runSprint(t, "add", title); code != 0 {
			t.Fatalf("sprint add: %s", out)
		}
		sid := string(rune('1' + i))
		if out, code := runSprint(t, "start", sid, "--owner", "Ada", "--for", "1h"); code != 0 {
			t.Fatalf("sprint start: %s", out)
		}
		if out, code := runSprint(t, "link", sid, "--repo", repo, "--task", sid); code != 0 {
			t.Fatalf("sprint link: %s", out)
		}
	}
	return root, dir
}

func TestSprintEndRefusesUndisposedRunThenClosesAtZeroResidue(t *testing.T) {
	root, dir := endCleanupFixture(t)
	out, code := runSprint(t, "end", "1")
	if code == 0 || !strings.Contains(out, "no disposition") || !strings.Contains(out, "weave pull 1") {
		t.Fatalf("end must refuse an undecided submitted run: exit=%d\n%s", code, out)
	}
	cache1 := weaveManagedGOCachePath(nil, dir, 1)
	if _, err := os.Stat(cache1); err != nil {
		t.Fatalf("refusal must not reclaim: %v", err)
	}
	// The conductor decides: the work is accepted by another route (a manual
	// merge), so the run is settled but nothing has swept its artifacts yet —
	// exactly what end must reclaim.
	gitT(t, root, "fetch", "-q", filepath.Join(dir, "workspaces", "issue-1"), "agent/weave-issue-1:agent/weave-issue-1")
	gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge run 1", "agent/weave-issue-1")
	out, code = runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("end after disposition: exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "residual: 0") || !strings.Contains(out, "reclaimed 1 workspace") || !strings.Contains(out, "1 cache") || !strings.Contains(out, "1 branch") {
		t.Fatalf("end must report what it reclaimed and assert residual 0:\n%s", out)
	}
	for _, p := range []string{cache1, filepath.Join(dir, "workspaces", "issue-1")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("sprint-owned artifact survived end: %s", p)
		}
	}
	if _, exists := gitBranchTip(root, "agent/weave-issue-1"); exists {
		t.Fatal("run #1 branch survived end")
	}
	q0, _ := loadWeaveQueue(dir)
	if it1 := findWeaveItem(q0, 1); it1.Disposition != weaveDispositionMerged || it1.Workspace != "" {
		t.Fatalf("run #1 row not compacted as merged: %+v", it1)
	}
	// The neighbour sprint's run on the SAME queue is untouched.
	q, _ := loadWeaveQueue(dir)
	it2 := findWeaveItem(q, 2)
	if it2 == nil || it2.State != "submitted" || it2.Workspace == "" {
		t.Fatalf("shared-queue run #2 was disturbed: %+v", it2)
	}
	if _, err := os.Stat(weaveManagedGOCachePath(nil, dir, 2)); err != nil {
		t.Fatalf("shared-queue run #2 cache reclaimed by a sprint that does not own it: %v", err)
	}
	if _, err := os.Stat(it2.Workspace); err != nil {
		t.Fatalf("shared-queue run #2 workspace gone: %v", err)
	}
}

func TestSprintEndRefusesOnRecordedCleanupFailure(t *testing.T) {
	_, dir := endCleanupFixture(t)
	if out, code := runWeave(t, "abandon", "1", "--yes", "--disposition", "superseded"); code != 0 {
		t.Fatalf("abandon: %s", out)
	}
	q, _ := loadWeaveQueue(dir)
	findWeaveItem(q, 1).CleanupError = "managed GOCACHE: permission denied"
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	out, code := runSprint(t, "end", "1")
	if code == 0 || !strings.Contains(out, "cleanup failed") {
		t.Fatalf("end must refuse on a recorded cleanup failure: exit=%d\n%s", code, out)
	}
}
