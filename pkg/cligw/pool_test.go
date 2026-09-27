package cligw

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPoolReplacesUsedWorkerToKeepMinSpare(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "warm")
	pool := NewPool(context.Background(), "test-agent", PoolConfig{
		StartServers: 1, MinSpare: 1, MaxSpare: 1, MaxWorkers: 2,
	})
	defer pool.Close()
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 1 })
	w, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Do(context.Background(), "pool", nil); err != nil {
		t.Fatal(err)
	}
	pool.Release(w)
	waitFor(t, 3*time.Second, func() bool {
		stats := pool.Stats()
		return stats.Idle == 1 && stats.Busy == 0 && stats.Spawned >= 2
	})
}

func TestPoolMaxWorkersQueuesFIFOAndHonorsContext(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "warm")
	pool := NewPool(context.Background(), "test-agent", PoolConfig{
		StartServers: 1, MinSpare: 0, MaxSpare: 1, MaxWorkers: 1,
	})
	defer pool.Close()
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 1 })
	first, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = pool.Acquire(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Acquire error = %v", err)
	}
	if stats := pool.Stats(); stats.Busy != 1 || stats.Queued != 0 || stats.Spawned != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	pool.Release(first)
}

func TestPoolIdleTTLRetiresOldestAboveMinSpare(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "warm")
	pool := NewPool(context.Background(), "test-agent", PoolConfig{
		StartServers: 1, MinSpare: 0, MaxSpare: 1, MaxWorkers: 1, IdleTTL: 60 * time.Millisecond,
	})
	defer pool.Close()
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 1 })
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 0 })
}

// A door stop cancels the server context before it closes each pool, so the
// pool's own context watcher usually starts Close first. The caller's Close
// must still return only once every worker is dead and its directory gone:
// the door exits right after it (Sprint 317, todo 369bc721).
func TestPoolCloseAfterContextCancelWaitsForWorkerCleanup(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "warm")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewPool(ctx, "test-agent", PoolConfig{
		StartServers: 3, MinSpare: 3, MaxSpare: 3, MaxWorkers: 3,
	})
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 3 })
	cancel()
	// Spin, not poll: Close must be entered while the watcher is still
	// killing workers.
	for deadline := time.Now().Add(3 * time.Second); ; {
		pool.mu.Lock()
		closed := pool.closed
		pool.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("context cancel did not close the pool")
		}
	}
	_ = pool.Close()
	left, err := filepath.Glob(filepath.Join(tmp, "cligw-worker-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("Close returned with %d worker dirs left: %v", len(left), left)
	}
}
