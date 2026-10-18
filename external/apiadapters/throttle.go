package apiadapters

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/external/gws"
	"github.com/qiangli/yoke/external/saasclients"
)

// Throttle gates request dispatch to a minimum interval between calls
// (client-side throttling). A nil *Throttle disables gating. The default
// intervals below follow each provider's documented rate limits; tests pass
// a millisecond-scale throttle or nil.
type Throttle struct {
	mu   sync.Mutex
	min  time.Duration
	last time.Time
}

// NewThrottle returns a gate enforcing at least min between dispatches.
func NewThrottle(min time.Duration) *Throttle { return &Throttle{min: min} }

// Wait blocks until min has elapsed since the previous dispatch (or returns
// promptly when the gate is nil or the context ends first).
func (t *Throttle) Wait(ctx context.Context) error {
	if t == nil || t.min <= 0 {
		return nil
	}
	t.mu.Lock()
	wait := t.min - time.Since(t.last)
	t.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// mark records a dispatch.
func (t *Throttle) mark() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
}

// Retry bounds.
const (
	// DefaultAttempts caps one read's provider attempts (initial + retries).
	DefaultAttempts = 3
	// maxRetryAfter caps an honored provider Retry-After so a hostile
	// header cannot park a short-lived client indefinitely.
	maxRetryAfter = 30 * time.Second
)

// CheckCursor validates a provider page cursor against the binding it was
// issued for. Provider cursors travel verbatim (this package never parses
// them: cursor serialization is owned by 446 S19); the binding pins the
// (target, query, revision) the cursor belongs to. An empty cursor starts
// a walk. A cursor presented under a different binding is a typed
// cursor-mismatch with an explicit restart choice — never silent replay.
func CheckCursor(b, issuedFor gws.Binding, cursor string) *gws.Error {
	if cursor == "" {
		return nil
	}
	return b.Validate(cursor, issuedFor)
}

// doRead performs one throttled read with bounded retries. Only safe reads
// retry, and only on retryable failures (rate-limit, timeout, provider
// failure); the provider's Retry-After is honored up to maxRetryAfter,
// otherwise attempts back off exponentially. A POST transport is allowed
// only for provider-defined read operations (Notion database query).
// Mutations must call api.Do (via doMutate) directly: an ambiguous write
// is never retried.
func doRead(ctx context.Context, api saasclients.API, t *Throttle, method, path string, q map[string]string, body []byte, headers map[string]string, attempts int) (*saasclients.Response, *gws.Error) {
	if attempts <= 0 {
		attempts = DefaultAttempts
	}
	query := url.Values{}
	for k, v := range q {
		if v != "" {
			query.Set(k, v)
		}
	}
	var err *gws.Error
	for n := 0; n < attempts; n++ {
		if werr := t.Wait(ctx); werr != nil {
			return nil, gws.NewError(gws.CodeCancelled, "apiadapters: cancelled; remote outcome unknown", nil)
		}
		t.mark()
		var r *saasclients.Response
		r, err = api.Do(ctx, method, path, query, body, headers)
		if err == nil {
			return r, nil
		}
		if !err.Retryable || n == attempts-1 {
			return r, err
		}
		if werr := sleepRetry(ctx, err, n); werr != nil {
			return nil, gws.NewError(gws.CodeCancelled, "apiadapters: cancelled during backoff; remote outcome unknown", nil)
		}
	}
	return nil, err
}

// doMutate performs one throttled mutating request with NO retry: a repeat
// may double-apply (creates without a provider key) or is simply wasteful.
// Callers needing replay safety hold the saasclients.Ledger.
func doMutate(ctx context.Context, api saasclients.API, t *Throttle, method, path string, body []byte, headers map[string]string) (*saasclients.Response, *gws.Error) {
	if werr := t.Wait(ctx); werr != nil {
		return nil, gws.NewError(gws.CodeCancelled, "apiadapters: cancelled; remote outcome unknown", nil)
	}
	t.mark()
	return api.Do(ctx, method, path, nil, body, headers)
}

// sleepRetry waits out a retryable failure: the provider's Retry-After when
// present and sane, else exponential backoff (200ms << n, capped at 2s).
func sleepRetry(ctx context.Context, err *gws.Error, n int) error {
	wait := 200 * time.Millisecond << n
	if wait > 2*time.Second {
		wait = 2 * time.Second
	}
	if err.Code == gws.CodeRateLimit && err.RetryAfter != "" {
		if s, convErr := strconv.Atoi(strings.TrimSpace(err.RetryAfter)); convErr == nil && s >= 0 {
			wait = time.Duration(s) * time.Second
			if wait > maxRetryAfter {
				wait = maxRetryAfter
			}
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
