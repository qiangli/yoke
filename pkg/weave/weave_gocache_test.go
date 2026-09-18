package weave

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unsetEnvForTest(t *testing.T, name string) {
	t.Helper()
	if cur, ok := os.LookupEnv(name); ok {
		t.Cleanup(func() { _ = os.Setenv(name, cur) })
	} else {
		t.Cleanup(func() { _ = os.Unsetenv(name) })
	}
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
}

func TestWeaveChildEnvManagedGOCacheReusedAcrossResume(t *testing.T) {
	queueDir := t.TempDir()
	ambient := []string{"PATH=/usr/bin"}
	it := &weaveItem{ID: 42, Title: "t", Body: "b", Owner: "codex-a"}

	first := weaveChildEnv(ambient, "/ws/issue-42", "agent/issue-42", "main", queueDir, it, nil)
	second := weaveChildEnv(ambient, "/ws/issue-42", "agent/issue-42", "main", queueDir, it, nil)
	other := weaveChildEnv(ambient, "/ws/issue-77", "agent/issue-77", "main", queueDir, &weaveItem{ID: 77, Title: "t", Body: "b", Owner: "codex-b"}, nil)

	cacheA, ok := envVal(first, weaveManagedGOCacheEnv)
	if !ok {
		t.Fatalf("first launch missing %s", weaveManagedGOCacheEnv)
	}
	cacheB, _ := envVal(second, weaveManagedGOCacheEnv)
	cacheC, _ := envVal(other, weaveManagedGOCacheEnv)
	if cacheA != cacheB {
		t.Fatalf("resume changed managed GOCACHE: first=%q second=%q", cacheA, cacheB)
	}
	if cacheA == cacheC {
		t.Fatalf("different runs share managed GOCACHE %q", cacheA)
	}
	if strings.Contains(cacheA, "/ws/issue-42") || !strings.HasPrefix(cacheA, queueDir) {
		t.Fatalf("managed GOCACHE must live under the queue dir, outside the workspace: %q", cacheA)
	}
}

func TestWeaveManagedGOCacheRespectsExplicitOverride(t *testing.T) {
	queueDir := t.TempDir()
	ambient := []string{"PATH=/usr/bin", weaveManagedGOCacheEnv + "=/explicit/cache"}
	it := &weaveItem{ID: 5, Title: "t", Body: "b", Owner: "codex-a"}

	child := weaveChildEnv(ambient, "/ws/issue-5", "agent/issue-5", "main", queueDir, it, nil)
	cache, ok := envVal(child, weaveManagedGOCacheEnv)
	if !ok || cache != "/explicit/cache" {
		t.Fatalf("child env GOCACHE = %q ok=%v, want explicit override preserved", cache, ok)
	}
	verifyEnv := weaveVerifyEnv(ambient, "/ws/issue-5", queueDir, it)
	cache, ok = envVal(verifyEnv, weaveManagedGOCacheEnv)
	if !ok || cache != "/explicit/cache" {
		t.Fatalf("verify env GOCACHE = %q ok=%v, want explicit override preserved", cache, ok)
	}
}

func TestRunWeaveReverifyUsesManagedGOCacheAndCleansItUp(t *testing.T) {
	unsetEnvForTest(t, weaveManagedGOCacheEnv)

	root := weaveTestRepo(t)
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
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, workspace, "add", ".")
	weaveTestGit(t, workspace, "commit", "-qm", "manual")

	verifyPathFile := filepath.Join(workspace, "managed-gocache.txt")
	q := &weaveQueue{
		NextID: 2,
		Root:   root,
		Items: []*weaveItem{{
			ID:            1,
			Title:         "managed go cache",
			State:         "submitted",
			Workspace:     workspace,
			Branch:        "agent/weave-issue-1",
			Created:       time.Now().UTC(),
			VerifyCommand: `cache=$(go env GOCACHE); mkdir -p "$cache"; printf %s "$cache" > managed-gocache.txt`,
		}},
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	oldWD, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "reverify", "1", "--json"); code != 0 {
		t.Fatalf("reverify exit=%d: %s", code, out)
	}

	wantCache := weaveManagedGOCachePath(nil, dir, 1)
	gotCacheBytes, err := os.ReadFile(verifyPathFile)
	if err != nil {
		t.Fatalf("read verify output: %v", err)
	}
	if got := string(gotCacheBytes); got != wantCache {
		t.Fatalf("verify observed GOCACHE %q, want %q", got, wantCache)
	}
	if _, err := os.Stat(wantCache); !os.IsNotExist(err) {
		t.Fatalf("managed GOCACHE still exists after coherent reverify: err=%v", err)
	}
}

func TestRunWeavePruneKeepsManagedGOCacheForActiveRun(t *testing.T) {
	root := weaveTestRepo(t)
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	cache := weaveManagedGOCachePath(nil, dir, 1)
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	q := &weaveQueue{
		NextID: 2,
		Root:   root,
		Items: []*weaveItem{{
			ID:         1,
			Title:      "active",
			State:      "working",
			Workspace:  filepath.Join(dir, "workspaces", "issue-1"),
			WrapperPid: os.Getpid(),
			Created:    time.Now().UTC(),
		}},
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	oldWD, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "prune", "--yes", "--json"); code != 0 {
		t.Fatalf("prune exit=%d: %s", code, out)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("active run cache was removed: %v", err)
	}
}

func TestRunWeavePruneRemovesCrashLeftoverManagedGOCache(t *testing.T) {
	root := weaveTestRepo(t)
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	cache := weaveManagedGOCachePath(nil, dir, 77)
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, &weaveQueue{NextID: 1, Root: root}); err != nil {
		t.Fatal(err)
	}

	oldWD, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "prune", "--yes", "--json")
	if code != 0 {
		t.Fatalf("prune exit=%d: %s", code, out)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("crash-leftover cache still exists: %v", err)
	}
	var doc struct {
		Result struct {
			Removed      int `json:"removed"`
			CacheRemoved int `json:"cache_removed"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode prune json: %v\n%s", err, out)
	}
	if doc.Result.Removed != 0 || doc.Result.CacheRemoved != 1 {
		t.Fatalf("prune counts = workspaces:%d caches:%d, want 0/1: %s",
			doc.Result.Removed, doc.Result.CacheRemoved, out)
	}
}
