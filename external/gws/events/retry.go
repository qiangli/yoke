package events

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

// Backoff is a bounded exponential retry policy for SAFE provider reads
// (polls, lease lookups). Downstream Handler actions are never retried here:
// an ambiguous write is not retried blindly.
type Backoff struct {
	// Base is the first delay; each further attempt doubles it.
	Base time.Duration
	// Max caps a single delay (and a provider retry-after).
	Max time.Duration
	// Attempts is the total number of tries, including the first.
	Attempts int
}

// DefaultBackoff: 4 tries, 1s → 2s → 4s, capped at 30s.
var DefaultBackoff = Backoff{Base: time.Second, Max: 30 * time.Second, Attempts: 4}

// Sleep waits d or until ctx is done. It is the one wait used by retries and
// the rate limiter, so cancellation interrupts every backoff.
type Sleep func(ctx context.Context, d time.Duration) error

func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// delay is the wait before retry n (0-based), honoring a provider
// retry-after when it is longer.
func (b Backoff) delay(n int, retryAfter string) time.Duration {
	d := b.Base << n
	if d <= 0 || d > b.Max {
		d = b.Max
	}
	if ra := parseRetryAfter(retryAfter); ra > d {
		d = ra
		if d > b.Max {
			d = b.Max
		}
	}
	return d
}

// parseRetryAfter accepts integer seconds or a Go duration.
func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return 0
}

// retry runs fn until it succeeds, fails non-retryably, exhausts the policy,
// or ctx is cancelled. Only *gws.Error values marked Retryable are retried.
func retry(ctx context.Context, b Backoff, sleep Sleep, fn func() error) error {
	attempts := b.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for n := 0; n < attempts; n++ {
		if cerr := ctx.Err(); cerr != nil {
			return cancelled(cerr)
		}
		if err = fn(); err == nil {
			return nil
		}
		var ge *gws.Error
		if !errors.As(err, &ge) || !ge.Retryable || n == attempts-1 {
			return err
		}
		if serr := sleep(ctx, b.delay(n, ge.RetryAfter)); serr != nil {
			return cancelled(serr)
		}
	}
	return err
}

func cancelled(err error) error {
	code := gws.CodeCancelled
	if errors.Is(err, context.DeadlineExceeded) {
		code = gws.CodeTimeout
	}
	e := gws.NewError(code, "gws events: "+err.Error()+" — checkpoint left at the last acknowledged event", nil)
	e.Retryable = false
	return e
}

// Limiter spaces provider calls at least Interval apart (a client-side rate
// limit under the provider quota). The zero Limiter does not wait.
type Limiter struct {
	Interval time.Duration
	Now      func() time.Time

	mu   sync.Mutex
	last time.Time
}

// Wait blocks until the next call is allowed or ctx is done.
func (l *Limiter) Wait(ctx context.Context, sleep Sleep) error {
	if l == nil || l.Interval <= 0 {
		return ctx.Err()
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	l.mu.Lock()
	t := now()
	wait := time.Duration(0)
	if !l.last.IsZero() {
		wait = l.last.Add(l.Interval).Sub(t)
	}
	if wait < 0 {
		wait = 0
	}
	l.last = t.Add(wait)
	l.mu.Unlock()
	if wait == 0 {
		return ctx.Err()
	}
	return sleep(ctx, wait)
}
