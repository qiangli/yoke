package sched

import (
	"sync"
	"sync/atomic"
)

// Phase D — Per-(backend, model) dispatch slot.
//
// The slot table is the scheduler's authoritative view of how many slots
// each (backend, model) is "burning" right now. Phase A's capacity
// probe tells us the ceiling (max_parallel). The slot counter
// guards against the picker piling too much work onto one backend
// between probe TTLs: even if the cached load looks low, we count
// our own in-flight at dispatch time so we don't overshoot.
//
// V1 keeps the surface small:
//   - Acquire is non-blocking. Either it gets a slot (returns
//     true) or it doesn't (returns false). The caller can try
//     another candidate via the failover slate, or surface a 503 +
//     retry-free nonce.
//   - Release is called from the modify-response tail so the
//     slot is held for the duration of the upstream stream.
//
// The full priority-heap dispatcher loop the doc describes is
// deferred until contention warrants it; what's here delivers the
// "cluster, not daemon, is the queue" semantic for the common case.

// DefaultDispatchSlotMax is the per-(backend, model) ceiling used when
// the caller has no backend-specific capacity hint.
const DefaultDispatchSlotMax = 4

// slotKey is the (backend, model) composite key. Stored in a map
// because the cardinality is small (backends * models in pool) and
// stable; an atomic.Int64 per key is read/written via the inner
// map.
type slotKey struct {
	Backend string
	Model   string
}

// SlotTable tracks per-(backend, model) in-flight counters. RWMutex
// guards the map mutation path; the counter itself is atomic so
// readers don't take the lock during steady-state acquire/release.
type SlotTable struct {
	mu     sync.RWMutex
	counts map[slotKey]*atomic.Int64
}

// NewSlotTable returns an isolated per-(backend, model) slot table.
func NewSlotTable() *SlotTable {
	return &SlotTable{counts: map[slotKey]*atomic.Int64{}}
}

func (t *SlotTable) ref(k slotKey) *atomic.Int64 {
	t.mu.RLock()
	v, ok := t.counts[k]
	t.mu.RUnlock()
	if ok {
		return v
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if v, ok := t.counts[k]; ok {
		return v
	}
	v = &atomic.Int64{}
	t.counts[k] = v
	return v
}

// Acquire tries to take one slot for (backend, model). Returns
// true on success. Non-blocking; the caller decides whether to
// fall back to another candidate or surface 503.
//
// maxParallel is the ceiling; zero (or negative) means "use the
// system default." Callers typically obtain it from a backend
// capacity probe.
func (t *SlotTable) Acquire(backend, model string, maxParallel int) bool {
	if maxParallel <= 0 {
		maxParallel = DefaultDispatchSlotMax
	}
	c := t.ref(slotKey{Backend: backend, Model: model})
	for {
		cur := c.Load()
		if cur >= int64(maxParallel) {
			return false
		}
		if c.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release decrements one (backend, model) slot. Safe to call on a
// key we never acquired (atomic decrement from zero is a no-op
// guard); the surrounding handler still uses defer.
func (t *SlotTable) Release(backend, model string) {
	c := t.ref(slotKey{Backend: backend, Model: model})
	// Clamp at zero — paranoid against accidental double-release.
	for {
		cur := c.Load()
		if cur <= 0 {
			return
		}
		if c.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// InFlight reads the live counter.
func (t *SlotTable) InFlight(backend, model string) int64 {
	c := t.ref(slotKey{Backend: backend, Model: model})
	return c.Load()
}
