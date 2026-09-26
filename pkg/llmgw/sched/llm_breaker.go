package sched

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Breaker is a shared per-backend circuit-breaker and cooldown. A backend that
// errors before the first response byte — transport failure, 5xx, or a 429
// rate-limit — is put in a time-bounded cooldown; while cooling it is
// deprioritized in routing (pickLeastLoaded operates on the healthy subset
// first), so parallel callers don't retry-storm a known-bad backend.
//
// ADVISORY, never fail-closed: when EVERY candidate is cooling the router
// still uses them — a recent hiccup must never make a model unreachable.
type Breaker struct {
	mu    sync.Mutex
	until map[string]time.Time // backend -> cooldown expiry
	now   func() time.Time
}

// NewBreaker returns an isolated breaker using the wall clock.
func NewBreaker() *Breaker {
	return newBreaker(time.Now)
}

func newBreaker(now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{until: map[string]time.Time{}, now: now}
}

const (
	// breakerBaseCooldown is the cooldown for a transport/5xx failure (a
	// 429 uses its Retry-After when present). breakerJitter spreads
	// recovery so backends don't all un-cool at once (R4 jitter).
	breakerBaseCooldown = 20 * time.Second
	breakerJitter       = 10 * time.Second
	// breakerMaxCooldown caps a hostile/huge Retry-After so one bad header
	// can't sideline a backend for an unbounded time.
	breakerMaxCooldown = 5 * time.Minute
)

// Trip puts backend into cooldown for ~d (0 ⇒ base), plus jitter, clamped to
// breakerMaxCooldown. A longer existing cooldown is preserved.
func (b *Breaker) Trip(backend string, d time.Duration) {
	if backend == "" {
		return
	}
	if d <= 0 {
		d = breakerBaseCooldown
	}
	d += time.Duration(rand.Int64N(int64(breakerJitter) + 1))
	if d > breakerMaxCooldown {
		d = breakerMaxCooldown
	}
	exp := b.now().Add(d)
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.until[backend]; !ok || exp.After(cur) {
		b.until[backend] = exp
	}
}

// InCooldown reports whether backend is currently cooling (and lazily evicts
// an expired entry).
func (b *Breaker) InCooldown(backend string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	exp, ok := b.until[backend]
	if !ok {
		return false
	}
	if !b.now().Before(exp) {
		delete(b.until, backend)
		return false
	}
	return true
}

// FilterAvailable returns the candidates NOT currently cooling, order
// preserved. May be empty (all cooling) — callers then fall back to the
// full set so a model never becomes unreachable.
func (b *Breaker) FilterAvailable(candidates []string) []string {
	out := make([]string, 0, len(candidates))
	for _, h := range candidates {
		if !b.InCooldown(h) {
			out = append(out, h)
		}
	}
	return out
}

// ParseRetryAfter parses a Retry-After header (delta-seconds or HTTP-date)
// into a positive duration; 0 when absent/unparseable/past.
func ParseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
