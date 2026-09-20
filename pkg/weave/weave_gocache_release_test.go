package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sprint 224 S1: the managed build cache is released on EVERY terminal
// transition, not only when a verify command produced a verdict. The measured
// incident was a submitted run with no verify command holding 6.1 GiB.

func weaveGOCacheReleaseFixture(t *testing.T, state, verify string) (root, dir, cache string) {
	t.Helper()
	unsetEnvForTest(t, weaveManagedGOCacheEnv)
	root = weaveTestRepo(t)
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(dir, "workspaces", "issue-1")
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	weaveTestGit(t, workspace, "checkout", "-qb", "agent/weave-issue-1")
	q := &weaveQueue{
		NextID: 2,
		Root:   root,
		Items: []*weaveItem{{
			ID:            1,
			Title:         "cache release",
			State:         state,
			Workspace:     workspace,
			Branch:        "agent/weave-issue-1",
			Created:       time.Now().UTC(),
			VerifyCommand: verify,
		}},
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	cache = weaveManagedGOCachePath(nil, dir, 1)
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "blob"), []byte("reproducible\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, dir, cache
}

func TestWeaveReverifyWithoutVerifyCommandReleasesManagedGOCache(t *testing.T) {
	root, _, cache := weaveGOCacheReleaseFixture(t, "submitted", "")
	oldWD, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "reverify", "1", "--json"); code != 0 {
		t.Fatalf("reverify exit=%d: %s", code, out)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("managed GOCACHE survived a no-verify terminal transition: err=%v", err)
	}
}

func TestWeaveReleaseManagedGOCacheEveryTerminalState(t *testing.T) {
	for _, state := range []string{"submitted", "no-op", "failed", "killed"} {
		for _, verify := range []string{"", "true"} {
			t.Run(state+"/verify="+verify, func(t *testing.T) {
				_, dir, cache := weaveGOCacheReleaseFixture(t, state, verify)
				q, err := loadWeaveQueue(dir)
				if err != nil {
					t.Fatal(err)
				}
				var stderr strings.Builder
				weaveReleaseManagedGOCache(&stderr, "test", dir, q.Items[0])
				if _, err := os.Stat(cache); !os.IsNotExist(err) {
					t.Fatalf("cache retained: err=%v stderr=%s", err, stderr.String())
				}
				// Idempotent: a second release is a silent no-op.
				weaveReleaseManagedGOCache(&stderr, "test", dir, q.Items[0])
				if stderr.Len() != 0 {
					t.Fatalf("unexpected stderr: %s", stderr.String())
				}
			})
		}
	}
}

func TestWeaveReleaseManagedGOCacheRecordsFailureOnRowThenClears(t *testing.T) {
	_, dir, cache := weaveGOCacheReleaseFixture(t, "submitted", "")
	// Make the path a FILE: safeRemoveManagedGOCache refuses a non-directory,
	// which is the cheapest deterministic failure the containment check yields.
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	weaveReleaseManagedGOCache(&stderr, "test", dir, q.Items[0])
	if !strings.Contains(stderr.String(), "cleanup failed") {
		t.Fatalf("failure not surfaced: %q", stderr.String())
	}
	fresh, _ := loadWeaveQueue(dir)
	if fresh.Items[0].CleanupError == "" || !strings.Contains(fresh.Items[0].CleanupError, "not a directory") {
		t.Fatalf("CleanupError not persisted on the row: %+v", fresh.Items[0])
	}
	// The operator fixes the cause; the next release clears the recorded fact.
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	weaveReleaseManagedGOCache(&stderr, "test", dir, fresh.Items[0])
	fresh, _ = loadWeaveQueue(dir)
	if fresh.Items[0].CleanupError != "" {
		t.Fatalf("CleanupError not cleared after successful release: %q", fresh.Items[0].CleanupError)
	}
}
