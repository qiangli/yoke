package sched

import (
	"sync"
	"time"
)

// HistoryBufferCap caps the number of recent (key → resolution)
// entries we keep. In-memory only; process restart drops the buffer
// and the classifier falls back to the heuristic stage until the
// fleet warms it back up. Sized for ~1 day of low-volume traffic
// at this fleet scale.
const HistoryBufferCap = 10000

// HistoryStaleTTL drops entries that haven't been confirmed-good
// recently. Stale entries don't poison routing — they just stop
// influencing it.
const HistoryStaleTTL = 7 * 24 * time.Hour

// HistoryRecord is what the buffer remembers about one past resolution.
type HistoryRecord struct {
	Tier     int
	Domains  []string
	Resolved string
	Success  bool
	At       time.Time
}

// HistoryBuffer is a tiny FIFO bounded map. We don't need true LRU
// here — the working set rolls over naturally as new prompts come
// in. Concurrent access is mutex-guarded; the hot path is read-mostly.
type HistoryBuffer struct {
	mu      sync.Mutex
	entries map[string]HistoryRecord
	order   []string // insertion order for FIFO eviction
}

// NewHistoryBuffer returns an isolated bounded history buffer.
func NewHistoryBuffer() *HistoryBuffer {
	return &HistoryBuffer{entries: map[string]HistoryRecord{}}
}

// Lookup returns the recorded resolution for one prefix-hash key.
// Returns false when missing or stale (older than HistoryStaleTTL).
func (h *HistoryBuffer) Lookup(key string) (HistoryRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.entries[key]
	if !ok {
		return HistoryRecord{}, false
	}
	if time.Since(rec.At) > HistoryStaleTTL {
		delete(h.entries, key)
		return HistoryRecord{}, false
	}
	return rec, true
}

// Record stores a successful resolution. Drops the oldest entry
// when at capacity. Skipped silently for empty key (untracked
// classification path) or empty resolved string (the auto path
// never picked one).
func (h *HistoryBuffer) Record(key, resolved string, tier int, doms []string, success bool) {
	if key == "" || resolved == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.entries) >= HistoryBufferCap {
		if len(h.order) > 0 {
			delete(h.entries, h.order[0])
			h.order = h.order[1:]
		}
	}
	h.entries[key] = HistoryRecord{
		Tier:     tier,
		Domains:  append([]string(nil), doms...),
		Resolved: resolved,
		Success:  success,
		At:       time.Now(),
	}
	h.order = append(h.order, key)
}

// Len returns the live entry count.
func (h *HistoryBuffer) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.entries)
}
