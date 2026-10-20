package weave

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sharedCacheFixture(t *testing.T) (state, queue string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	unsetEnvForTest(t, weaveManagedGOCacheEnv)
	state = t.TempDir()
	queue = filepath.Join(state, "repo-aaaa")
	if err := os.MkdirAll(queue, 0o755); err != nil {
		t.Fatal(err)
	}
	return state, queue
}

func TestSharedGOCacheSameAcrossRunsAndQueues(t *testing.T) {
	state, q1 := sharedCacheFixture(t)
	q2 := filepath.Join(state, "other-bbbb")
	a := weaveSharedGOCachePath(nil, q1, 1)
	b := weaveSharedGOCachePath(nil, q1, 2)
	c := weaveSharedGOCachePath(nil, q2, 3)
	if a == "" || a != b || a != c {
		t.Fatalf("runs must share one GOCACHE: %q %q %q", a, b, c)
	}
}

func TestSharedGOCacheParentEnvWins(t *testing.T) {
	_, q := sharedCacheFixture(t)
	if got := weaveSharedGOCachePath([]string{"GOCACHE=/mine"}, q, 1); got != "" {
		t.Fatalf("parent GOCACHE must win, got %q", got)
	}
}

func fillSharedCache(t *testing.T, q string, n int) string {
	root := filepath.Join(filepath.Dir(q), weaveSharedGOCacheDir)
	if err := os.MkdirAll(filepath.Join(root, "ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		p := filepath.Join(root, "ab", string(rune('a'+i))+"-d")
		if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(p, mt, mt)
	}
	return root
}

func TestSharedGOCacheTrimKeepsUnderCapEvictingOldest(t *testing.T) {
	_, q := sharedCacheFixture(t)
	root := fillSharedCache(t, q, 10)
	old := weaveGOCacheHostLive
	weaveGOCacheHostLive = func(string) bool { return false }
	t.Cleanup(func() { weaveGOCacheHostLive = old })
	freed, err := weaveTrimSharedGOCache(q, 450)
	if err != nil || freed != 600 {
		t.Fatalf("freed=%d err=%v, want 600", freed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "ab", "a-d")); !os.IsNotExist(err) {
		t.Fatalf("oldest entry survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ab", "j-d")); err != nil {
		t.Fatalf("newest entry evicted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ab")); err != nil {
		t.Fatalf("cache directory removed: %v", err)
	}
}

func ageSharedCache(t *testing.T, q string, ages map[string]time.Duration) string {
	t.Helper()
	root := filepath.Join(filepath.Dir(q), weaveSharedGOCacheDir)
	if err := os.MkdirAll(filepath.Join(root, "ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, age := range ages {
		p := filepath.Join(root, "ab", name)
		if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		_ = os.Chtimes(p, mt, mt)
	}
	return root
}

func setHostLive(t *testing.T, live bool) {
	old := weaveGOCacheHostLive
	weaveGOCacheHostLive = func(string) bool { return live }
	t.Cleanup(func() { weaveGOCacheHostLive = old })
}

func TestSharedGOCacheTrimWhileLiveEvictsOnlyOldFiles(t *testing.T) {
	_, q := sharedCacheFixture(t)
	root := ageSharedCache(t, q, map[string]time.Duration{
		"old1": 5 * time.Hour, "old2": 3 * time.Hour, "old3": 2 * time.Hour,
		"new1": 10 * time.Minute, "new2": time.Minute,
	})
	setHostLive(t, true)
	freed, err := weaveTrimSharedGOCache(q, 100)
	if err != nil || freed != 300 {
		t.Fatalf("freed=%d err=%v, want 300 (all old files, no new)", freed, err)
	}
	for _, n := range []string{"old1", "old2", "old3"} {
		if _, err := os.Stat(filepath.Join(root, "ab", n)); !os.IsNotExist(err) {
			t.Fatalf("%s survived: %v", n, err)
		}
	}
	for _, n := range []string{"new1", "new2"} {
		if _, err := os.Stat(filepath.Join(root, "ab", n)); err != nil {
			t.Fatalf("recent %s evicted while live: %v", n, err)
		}
	}
}

func TestSharedGOCacheTrimWhileLiveStopsAtCap(t *testing.T) {
	_, q := sharedCacheFixture(t)
	root := ageSharedCache(t, q, map[string]time.Duration{
		"old1": 5 * time.Hour, "old2": 3 * time.Hour, "new1": time.Minute,
	})
	setHostLive(t, true)
	freed, _ := weaveTrimSharedGOCache(q, 200)
	if freed != 100 {
		t.Fatalf("freed=%d, want 100", freed)
	}
	if _, err := os.Stat(filepath.Join(root, "ab", "old2")); err != nil {
		t.Fatalf("evicted past the cap: %v", err)
	}
}

func TestSharedGOCacheTrimWhileLiveKeepsRecentEvenOverCap(t *testing.T) {
	_, q := sharedCacheFixture(t)
	root := fillSharedCache(t, q, 10) // all younger than the safety age except the oldest
	setHostLive(t, true)
	freed, _ := weaveTrimSharedGOCache(q, 0)
	if freed > 100 {
		t.Fatalf("freed=%d, recent files must survive", freed)
	}
	if _, err := os.Stat(filepath.Join(root, "ab", "j-d")); err != nil {
		t.Fatal(err)
	}
}

func TestSharedGOCacheMaxBytesEnv(t *testing.T) {
	if weaveGOCacheMaxBytes() != 15<<30 {
		t.Fatal("default cap")
	}
	t.Setenv(weaveGOCacheMaxEnv, "1234")
	if weaveGOCacheMaxBytes() != 1234 {
		t.Fatal("env cap")
	}
}

func TestLegacyPerRunGOCachesMigratedOnlyWhenNotLive(t *testing.T) {
	_, q := sharedCacheFixture(t)
	dead := weaveLegacyGOCachePath(nil, q, 1)
	live := weaveLegacyGOCachePath(nil, q, 2)
	for _, p := range []string{dead, live} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	err := saveWeaveQueue(q, &weaveQueue{NextID: 3, Items: []*weaveItem{
		{ID: 1, State: "failed", Created: time.Now().UTC()},
		{ID: 2, State: "working", WrapperPid: os.Getpid(), Created: time.Now().UTC()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	weaveMigrateLegacyGOCaches(q)
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("legacy cache of terminal run survived: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("legacy cache of live run removed: %v", err)
	}
}
