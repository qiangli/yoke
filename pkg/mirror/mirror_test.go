package mirror

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A change in a NESTED subdir triggers a (debounced) sync — proving the recursive
// watch + the event→debounce→sync loop. Hermetic: injected Sync, no rclone.
func TestMirror_RecursiveWatchTriggersSync(t *testing.T) {
	dir := t.TempDir()
	var syncs int32
	o := Options{
		Source:   dir,
		Dest:     t.TempDir(),
		Debounce: 100 * time.Millisecond,
		Interval: 0, // no periodic backstop — isolate the watcher path
		Sync:     func(context.Context) error { atomic.AddInt32(&syncs, 1); return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()
	defer func() { cancel(); <-done }()

	// Initial sync happens up front.
	waitFor(t, func() bool { return atomic.LoadInt32(&syncs) >= 1 }, time.Second, "initial sync")
	base := atomic.LoadInt32(&syncs)

	// Change in a nested subdir (exercises recursion).
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return atomic.LoadInt32(&syncs) > base }, 2*time.Second,
		"sync after nested change")
}

func TestMirror_RequiresValidSource(t *testing.T) {
	if err := Run(context.Background(), Options{Source: "", Dest: "x"}); err == nil {
		t.Fatal("expected error for empty source")
	}
	if err := Run(context.Background(), Options{Source: filepath.Join(t.TempDir(), "nope"), Dest: "x"}); err == nil {
		t.Fatal("expected error for non-existent source dir")
	}
}

func waitFor(t *testing.T, cond func() bool, max time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
