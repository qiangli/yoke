package gateway

import (
	"context"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultLoadedPollInterval is the standard refresh cadence.
	DefaultLoadedPollInterval = 10 * time.Second
	// DefaultLoadedTTL keeps one missed refresh from discarding a useful hint.
	DefaultLoadedTTL = 35 * time.Second
)

type loadedEntry struct {
	models map[string]struct{}
	at     time.Time
}

// LoadedCache caches loaded-model observations by backend name.
type LoadedCache struct {
	mu       sync.RWMutex
	ttl      time.Duration
	interval time.Duration
	byName   map[string]loadedEntry
	now      func() time.Time
}

// NewLoadedCache returns an empty cache. Non-positive durations use defaults.
func NewLoadedCache(ttl, interval time.Duration) *LoadedCache {
	if ttl <= 0 {
		ttl = DefaultLoadedTTL
	}
	if interval <= 0 {
		interval = DefaultLoadedPollInterval
	}
	return &LoadedCache{ttl: ttl, interval: interval, byName: make(map[string]loadedEntry), now: time.Now}
}

// DefaultLoadedCache is suitable for applications that want one shared cache.
var DefaultLoadedCache = NewLoadedCache(DefaultLoadedTTL, DefaultLoadedPollInterval)

// TTL returns the configured observation lifetime.
func (c *LoadedCache) TTL() time.Duration { return c.ttl }

// Put records a loaded-model set for name. Keys are normalized on insertion.
func (c *LoadedCache) Put(name string, models map[string]struct{}) {
	normalized := make(map[string]struct{}, len(models))
	for model := range models {
		if model = normalizeModel(model); model != "" {
			normalized[model] = struct{}{}
		}
	}
	c.mu.Lock()
	c.byName[name] = loadedEntry{models: normalized, at: c.now()}
	c.mu.Unlock()
}

// HasLoaded reports whether a fresh observation contains model.
func (c *LoadedCache) HasLoaded(name, model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.byName[name]
	if !ok || c.now().Sub(entry.at) > c.ttl {
		return false
	}
	_, ok = entry.models[normalizeModel(model)]
	return ok
}

// PartitionLoaded stably splits candidates into loaded and other backends.
func (c *LoadedCache) PartitionLoaded(model string, candidates []Backend) (loaded, others []Backend) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	model = normalizeModel(model)
	now := c.now()
	for _, backend := range candidates {
		if backend == nil {
			others = append(others, backend)
			continue
		}
		entry, ok := c.byName[backend.Name()]
		if !ok || now.Sub(entry.at) > c.ttl {
			others = append(others, backend)
			continue
		}
		if _, ok := entry.models[model]; ok {
			loaded = append(loaded, backend)
		} else {
			others = append(others, backend)
		}
	}
	return loaded, others
}

// StartLoadedPoller starts an immediate refresh followed by periodic refreshes.
func (c *LoadedCache) StartLoadedPoller(ctx context.Context, backends func() []Backend) {
	tick := func() {
		for _, backend := range backends() {
			if backend == nil {
				continue
			}
			models, ok := backend.Loaded(ctx)
			if ok {
				c.Put(backend.Name(), models)
			}
		}
	}
	go func() {
		tick()
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tick()
			}
		}
	}()
}

// StartLoadedPoller starts a poller using DefaultLoadedCache.
func StartLoadedPoller(ctx context.Context, backends func() []Backend) {
	DefaultLoadedCache.StartLoadedPoller(ctx, backends)
}

func normalizeModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}
