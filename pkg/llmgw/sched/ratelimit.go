package sched

import (
	"sync"
	"time"
)

// LLMRateLimiter is the in-process per-(principal, UTC day) request
// counter used to gate requests. It is intentionally in-memory:
//
//   - Request rates are fast and high; persistent writes per request
//     would add latency and contention.
//   - A process restart resets every counter.
//   - Multi-replica deployments need a shared counter.
//
// Day rollover is computed lazily on every Check call: when the
// stored day differs from time.Now().UTC().Format("2006-01-02"), the
// counter resets to 1 (this request) regardless of the prior value.
type LLMRateLimiter struct {
	mu      sync.Mutex
	state   map[string]rateState // keyed by principal ID
	nowFunc func() time.Time     // overridable for tests
}

type rateState struct {
	day      string
	requests int
}

// NewLLMRateLimiter returns a fresh counter. Tests pass a nowFunc to
// drive day rollover deterministically; production passes nil and
// time.Now is used.
func NewLLMRateLimiter(nowFunc func() time.Time) *LLMRateLimiter {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &LLMRateLimiter{
		state:   map[string]rateState{},
		nowFunc: nowFunc,
	}
}

// Check increments the today-UTC counter for principalID and returns
// (allowed, currentUsage). When limit==0 the call is always allowed
// (unlimited) — the counter still ticks so current usage remains observable.
func (l *LLMRateLimiter) Check(principalID string, limit int) (allowed bool, usage int) {
	today := l.nowFunc().UTC().Format("2006-01-02")
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state[principalID]
	if s.day != today {
		s = rateState{day: today, requests: 0}
	}
	s.requests++
	l.state[principalID] = s
	if limit > 0 && s.requests > limit {
		return false, s.requests
	}
	return true, s.requests
}

// Usage returns the current today-UTC counter without incrementing.
func (l *LLMRateLimiter) Usage(principalID string) int {
	today := l.nowFunc().UTC().Format("2006-01-02")
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state[principalID]
	if s.day != today {
		return 0
	}
	return s.requests
}
