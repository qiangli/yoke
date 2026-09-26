package gateway

import (
	"context"
	"testing"
	"time"
)

func TestLoadedCacheHasLoadedAndPartitions(t *testing.T) {
	now := time.Unix(100, 0)
	cache := NewLoadedCache(time.Minute, time.Hour)
	cache.now = func() time.Time { return now }
	alpha := &fakeBackend{name: "alpha"}
	beta := &fakeBackend{name: "beta"}
	gamma := &fakeBackend{name: "gamma"}
	cache.Put("alpha", map[string]struct{}{" Model-A ": {}})
	cache.Put("beta", map[string]struct{}{"model-b": {}})
	if !cache.HasLoaded("alpha", "MODEL-A") {
		t.Fatal("case-insensitive loaded lookup missed")
	}
	loaded, others := cache.PartitionLoaded("model-a", []Backend{alpha, beta, gamma})
	if len(loaded) != 1 || loaded[0] != alpha || len(others) != 2 || others[0] != beta || others[1] != gamma {
		t.Fatalf("loaded=%v others=%v", names(loaded), names(others))
	}
	now = now.Add(cache.TTL() + time.Nanosecond)
	if cache.HasLoaded("alpha", "model-a") {
		t.Fatal("stale loaded observation remained fresh")
	}
}

func TestLoadedPollerRefreshesImmediately(t *testing.T) {
	called := make(chan struct{}, 1)
	backend := &fakeBackend{name: "alpha", loaded: map[string]struct{}{"model-a": {}}, loadedOK: true, loadedCall: called}
	cache := NewLoadedCache(time.Minute, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache.StartLoadedPoller(ctx, func() []Backend { return []Backend{backend} })
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("initial poll did not run")
	}
	for deadline := time.Now().Add(time.Second); !cache.HasLoaded("alpha", "model-a"); {
		if time.Now().After(deadline) {
			t.Fatal("initial poll did not update cache")
		}
		time.Sleep(time.Millisecond)
	}
}

func names(backends []Backend) []string {
	out := make([]string, len(backends))
	for i, backend := range backends {
		if backend != nil {
			out[i] = backend.Name()
		}
	}
	return out
}
