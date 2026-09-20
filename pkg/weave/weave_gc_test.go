package weave

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Sprint 224 S5. The fixture rebuilds the measured incident in a temp HOME:
// a queue whose repository was RENAMED (bashpp → bashsharp: metadata-only,
// its workspaces long gone), a queue whose repository is gone but whose
// workspace still holds an agent branch (must be refused), a live queue with a
// settled run, an orphan cache and a legacy -reviewed branch, and an unrelated
// ACTIVE queue that must come out byte-identical.

func dirDigest(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			lines = append(lines, "d "+rel)
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		lines = append(lines, "f "+rel+" "+hex.EncodeToString(sum[:8]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func gcFixture(t *testing.T) (home, live, liveRoot, active, renamed, goneUnique string) {
	t.Helper()
	unsetEnvForTest(t, weaveManagedGOCacheEnv)
	liveRoot = setupIsolationFixture(t) // sets HOME
	home, _ = os.UserHomeDir()
	live, _ = weaveQueueDir(liveRoot)

	// live queue: run 1 merged but unswept (+ cache), orphan cache run-9,
	// legacy reviewed branch for a run that no longer exists (issue 5).
	ws := filepath.Join(live, "workspaces", "issue-1")
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, liveRoot, "clone", "-q", liveRoot, ws)
	gitT(t, ws, "checkout", "-qb", "agent/weave-issue-1")
	writeAndCommit(t, ws, "one.txt", "1\n", "run one")
	gitT(t, liveRoot, "fetch", "-q", ws, "agent/weave-issue-1:agent/weave-issue-1")
	gitT(t, liveRoot, "merge", "-q", "--no-ff", "-m", "merge one", "agent/weave-issue-1")
	gitT(t, liveRoot, "checkout", "-qb", "agent/weave-issue-5-reviewed")
	writeAndCommit(t, liveRoot, "five.txt", "5\n", "five reviewed")
	gitT(t, liveRoot, "checkout", "-q", "main")
	gitT(t, liveRoot, "merge", "-q", "--ff", "agent/weave-issue-5-reviewed")
	if err := saveWeaveQueue(live, &weaveQueue{Root: liveRoot, NextID: 2, Items: []*weaveItem{{
		ID: 1, State: "submitted", Workspace: ws, Branch: "agent/weave-issue-1", CommitsAhead: 1, Created: time.Now().UTC(),
	}}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 9} {
		c := weaveManagedGOCachePath(nil, live, id)
		if err := os.MkdirAll(c, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(c, "blob"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// active queue: a working run with a live wrapper (this test process).
	activeRoot := filepath.Join(t.TempDir(), "active-repo")
	if err := os.MkdirAll(activeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, activeRoot, "init", "-q", "-b", "main")
	writeAndCommit(t, activeRoot, "seed", "seed\n", "seed")
	activeRoot, _ = weaveRepoRoot(activeRoot)
	active, _ = weaveQueueDir(activeRoot)
	aws := filepath.Join(active, "workspaces", "issue-1")
	if err := os.MkdirAll(filepath.Dir(aws), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, activeRoot, "clone", "-q", activeRoot, aws)
	if err := saveWeaveQueue(active, &weaveQueue{Root: activeRoot, NextID: 2, Items: []*weaveItem{{
		ID: 1, State: "working", Workspace: aws, WrapperPid: os.Getpid(), Created: time.Now().UTC(),
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(weaveManagedGOCachePath(nil, active, 1), 0o755); err != nil {
		t.Fatal(err)
	}

	// renamed: metadata-only queue for a repo path that no longer exists.
	renamed = filepath.Join(weaveStateRoot(home), "bashpp-deadbeef")
	if err := os.MkdirAll(filepath.Join(renamed, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(renamed, &weaveQueue{Root: filepath.Join(t.TempDir(), "bashpp"), NextID: 3, Items: []*weaveItem{
		{ID: 1, State: "done", Created: time.Now().UTC()},
		{ID: 2, State: "abandoned", Created: time.Now().UTC()},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(renamed, "logs", "issue-1.log"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// goneUnique: repo gone, workspace holds an agent branch that exists only there.
	goneUnique = filepath.Join(weaveStateRoot(home), "vanished-cafebabe")
	gws := filepath.Join(goneUnique, "workspaces", "issue-1")
	if err := os.MkdirAll(gws, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, gws, "init", "-q", "-b", "main")
	writeAndCommit(t, gws, "seed", "seed\n", "seed")
	gitT(t, gws, "checkout", "-qb", "agent/weave-issue-1")
	writeAndCommit(t, gws, "unique.txt", "only here\n", "unique work")
	if err := saveWeaveQueue(goneUnique, &weaveQueue{Root: filepath.Join(t.TempDir(), "vanished"), NextID: 2, Items: []*weaveItem{
		{ID: 1, State: "killed", Workspace: gws, Created: time.Now().UTC()},
	}}); err != nil {
		t.Fatal(err)
	}
	return
}

func TestWeaveGCReportThenApply(t *testing.T) {
	_, live, liveRoot, active, renamed, goneUnique := gcFixture(t)
	before := dirDigest(t, active)
	t.Chdir(liveRoot)

	out, code := runWeave(t, "gc")
	if code != 0 {
		t.Fatalf("gc report: %s", out)
	}
	for _, want := range []string{"would remove", "REFUSED workspace " + filepath.Join(goneUnique, "workspaces", "issue-1"), "state-root " + renamed, "agent/weave-issue-5-reviewed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
	// Report changed nothing.
	if _, err := os.Stat(renamed); err != nil {
		t.Fatalf("report removed the renamed root: %v", err)
	}
	if _, err := os.Stat(weaveManagedGOCachePath(nil, live, 9)); err != nil {
		t.Fatalf("report removed the orphan cache: %v", err)
	}

	out, code = runWeave(t, "gc", "--apply")
	if code != 0 {
		t.Fatalf("gc apply: %s", out)
	}
	for _, p := range []string{renamed, weaveManagedGOCachePath(nil, live, 9), weaveManagedGOCachePath(nil, live, 1), filepath.Join(live, "workspaces", "issue-1")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("not reclaimed: %s (%v)", p, err)
		}
	}
	for _, br := range []string{"agent/weave-issue-1", "agent/weave-issue-5-reviewed"} {
		if _, exists := gitBranchTip(liveRoot, br); exists {
			t.Fatalf("integrated branch %s survived gc", br)
		}
	}
	if _, err := os.Stat(filepath.Join(goneUnique, "workspaces", "issue-1", "unique.txt")); err != nil {
		t.Fatalf("unique work under a missing root was destroyed: %v", err)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Fatalf("apply must still name the refusal:\n%s", out)
	}
	if after := dirDigest(t, active); after != before {
		t.Fatalf("unrelated active queue changed:\n--- before\n%s\n--- after\n%s", before, after)
	}
	q, _ := loadWeaveQueue(live)
	if it := findWeaveItem(q, 1); it.State != "done" || it.Disposition != weaveDispositionMerged {
		t.Fatalf("ghost submitted row not reconciled by gc: %+v", it)
	}
	// Idempotent.
	out, _ = runWeave(t, "gc", "--apply")
	if strings.Contains(out, "removed ") && !strings.Contains(out, "0 item(s) removed") {
		t.Fatalf("second apply removed something:\n%s", out)
	}
}
