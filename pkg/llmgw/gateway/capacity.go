package gateway

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// DefaultCapacityTTL is the standard lifetime of a capacity observation.
const DefaultCapacityTTL = 3 * time.Second

// Capacity describes a backend's current parallel work and model state.
type Capacity struct {
	Version      int      `json:"version"`
	MaxParallel  int      `json:"max_parallel"`
	InFlight     int      `json:"in_flight"`
	Queued       int      `json:"queued"`
	LoadedModels []string `json:"loaded_models"`
	Swapping     bool     `json:"swapping"`
	NumLoadedMax int      `json:"num_loaded_max"`
	KeepAliveS   int      `json:"keep_alive_s"`
	MaxQueue     int      `json:"max_queue"`
	Loaded       float64  `json:"-"`
}

func (c *Capacity) setLoad() {
	if c.MaxParallel > 0 {
		c.Loaded = float64(c.InFlight) / float64(c.MaxParallel)
	}
	if c.Swapping && c.Loaded < 1 {
		c.Loaded = 1
	}
}

type capacityEntry struct {
	capacity Capacity
	at       time.Time
}

// CapacityCache caches capacity observations by backend name.
type CapacityCache struct {
	mu     sync.Mutex
	ttl    time.Duration
	byName map[string]capacityEntry
	now    func() time.Time
}

// NewCapacityCache returns an empty cache. A non-positive TTL uses the default.
func NewCapacityCache(ttl time.Duration) *CapacityCache {
	if ttl <= 0 {
		ttl = DefaultCapacityTTL
	}
	return &CapacityCache{ttl: ttl, byName: make(map[string]capacityEntry), now: time.Now}
}

// DefaultCapacityCache is suitable for applications that want one shared cache.
var DefaultCapacityCache = NewCapacityCache(DefaultCapacityTTL)

// TTL returns the configured observation lifetime.
func (c *CapacityCache) TTL() time.Duration { return c.ttl }

// Get returns a fresh observation for name.
func (c *CapacityCache) Get(name string) (Capacity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byName[name]
	if !ok || c.now().Sub(entry.at) > c.ttl {
		return Capacity{}, false
	}
	return entry.capacity, true
}

// Put records an observation for name at the current time.
func (c *CapacityCache) Put(name string, capacity Capacity) {
	capacity.setLoad()
	c.mu.Lock()
	c.byName[name] = capacityEntry{capacity: capacity, at: c.now()}
	c.mu.Unlock()
}

// PickLeastLoaded returns a least-loaded backend. Unknown observations sort
// behind known observations; equal loads are selected randomly.
func (c *CapacityCache) PickLeastLoaded(ctx context.Context, candidates []Backend) Backend {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	type scored struct {
		backend Backend
		known   bool
		load    float64
	}
	scores := make([]scored, 0, len(candidates))
	for _, backend := range candidates {
		if backend == nil {
			continue
		}
		capacity, fresh := c.Get(backend.Name())
		if !fresh {
			capacity = backend.Capacity(ctx)
			capacity.setLoad()
			if capacity.MaxParallel > 0 {
				c.Put(backend.Name(), capacity)
			}
		}
		scores = append(scores, scored{backend: backend, known: capacity.MaxParallel > 0, load: capacity.Loaded})
	}
	bestLoad := -1.0
	for _, score := range scores {
		if score.known && (bestLoad < 0 || score.load < bestLoad) {
			bestLoad = score.load
		}
	}
	pool := make([]Backend, 0, len(scores))
	for _, score := range scores {
		if bestLoad < 0 || score.known && score.load == bestLoad {
			pool = append(pool, score.backend)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	if len(pool) == 1 {
		return pool[0]
	}
	return pool[rand.IntN(len(pool))]
}
