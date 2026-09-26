package gateway

import (
	"context"
	"testing"
	"time"
)

func TestCapacityCacheGetPutAndTTL(t *testing.T) {
	now := time.Unix(100, 0)
	cache := NewCapacityCache(3 * time.Second)
	cache.now = func() time.Time { return now }
	cache.Put("alpha", Capacity{MaxParallel: 4, InFlight: 2})
	got, ok := cache.Get("alpha")
	if !ok || got.Loaded != 0.5 {
		t.Fatalf("get = %+v, %v", got, ok)
	}
	now = now.Add(cache.TTL() + time.Nanosecond)
	if _, ok := cache.Get("alpha"); ok {
		t.Fatal("stale capacity remained fresh")
	}
}

func TestCapacityCachePickLeastLoaded(t *testing.T) {
	cache := NewCapacityCache(time.Minute)
	busy := &fakeBackend{name: "busy", capacity: Capacity{MaxParallel: 4, InFlight: 3}}
	idle := &fakeBackend{name: "idle", capacity: Capacity{MaxParallel: 4}}
	unknown := &fakeBackend{name: "unknown"}
	for range 5 {
		if got := cache.PickLeastLoaded(context.Background(), []Backend{busy, unknown, idle}); got != idle {
			t.Fatalf("picked %v, want idle", got.Name())
		}
	}
}

func TestCapacityCachePrefersNonSwapping(t *testing.T) {
	cache := NewCapacityCache(time.Minute)
	swapping := &fakeBackend{name: "swapping", capacity: Capacity{MaxParallel: 4, Swapping: true}}
	warm := &fakeBackend{name: "warm", capacity: Capacity{MaxParallel: 4, InFlight: 2}}
	if got := cache.PickLeastLoaded(context.Background(), []Backend{swapping, warm}); got != warm {
		t.Fatalf("picked %v, want warm", got.Name())
	}
}
