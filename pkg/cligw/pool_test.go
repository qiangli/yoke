package cligw

import (
	"context"
	"errors"
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
