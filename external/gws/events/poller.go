package events

import (
	"context"
	"errors"
	"fmt"

	"github.com/qiangli/yoke/external/gws"
)

// DefaultMaxBatches bounds one RunOnce walk.
const DefaultMaxBatches = 20

// Poller drains a Source into a Handler with ordered acknowledgment. One
// RunOnce is a bounded invocation (it returns when caught up, at MaxBatches,
// on error, or on cancellation) — schedule it with the existing scheduler
// rather than running a resident loop, which would be a long-lived worker
// under service Rule 1.
type Poller struct {
	Source  Source
	Store   *Store
	Handler Handler
	// Backoff governs retries of Source.Poll (zero → DefaultBackoff).
	Backoff Backoff
	// Limiter spaces Source.Poll calls (nil → no client-side limit).
	Limiter *Limiter
	// MaxBatches bounds the walk (zero → DefaultMaxBatches).
	MaxBatches int
	// Secrets are literal values (tokens, credential file contents) masked
	// out of every returned error message.
	Secrets []string
	// Sleep is the cancellable wait (nil → timer); injectable for tests.
	Sleep Sleep
}

// Result reports one RunOnce. Partial is true when the walk stopped before
// catching up (bound, error, or cancellation) — never a silent truncation.
type Result struct {
	Batches    int    `json:"batches"`
	Delivered  int    `json:"delivered"`
	Duplicates int    `json:"duplicates"`
	Cursor     string `json:"cursor"`
	CaughtUp   bool   `json:"caught_up"`
	Partial    bool   `json:"partial"`
}

// RunOnce resumes from the persisted cursor and processes changes in order.
// Each event is acknowledged only after its Handler succeeds; the cursor
// advances only after the whole batch is acknowledged. On restart after a
// crash, the same batch is re-polled and already-acknowledged events are
// skipped as duplicates (at-least-once; no exactly-once promise).
func (p *Poller) RunOnce(ctx context.Context) (Result, error) {
	sleep := p.Sleep
	if sleep == nil {
		sleep = ctxSleep
	}
	backoff := p.Backoff
	if backoff.Attempts == 0 {
		backoff = DefaultBackoff
	}
	max := p.MaxBatches
	if max <= 0 {
		max = DefaultMaxBatches
	}
	cp := p.Store.Checkpoint()
	res := Result{Cursor: cp.Cursor, Partial: true}
	if cp.Cursor == "" {
		return res, gws.NewError(gws.CodeInvalidInput, "gws events: no starting cursor — run an explicit Resync (e.g. with the historyId returned by a watch)", nil).WithOp("poll", cp.Target)
	}
	cursor := cp.Cursor
	for res.Batches < max {
		if err := p.Limiter.Wait(ctx, sleep); err != nil {
			return res, cancelled(err)
		}
		var batch Batch
		err := retry(ctx, backoff, sleep, func() error {
			var perr error
			batch, perr = p.Source.Poll(ctx, cursor)
			return perr
		})
		if err != nil {
			return res, redactErr(err, cp.Target, p.Secrets)
		}
		res.Batches++
		for _, ev := range batch.Events {
			if err := ctx.Err(); err != nil {
				return res, cancelled(err)
			}
			if ev.ID == "" {
				return res, gws.NewError(gws.CodeProviderFailure, "gws events: event without an id cannot be deduplicated — batch not acknowledged", nil).WithOp("poll", cp.Target)
			}
			if p.Store.Seen(ev.ID) {
				res.Duplicates++
				continue
			}
			if err := p.Handler(ctx, ev); err != nil {
				// The downstream outcome is unknown to us: report it as such,
				// leave the event unacknowledged, and never retry it here.
				msg := fmt.Sprintf("gws events: handler failed on event %s; it stays unacknowledged and will be redelivered: %v", ev.ID, err)
				return res, redactErr(gws.NewError(gws.CodePartialMutation, msg, nil).WithOp("handle", cp.Target), cp.Target, p.Secrets)
			}
			if err := p.Store.Ack(ev.ID); err != nil {
				return res, err
			}
			res.Delivered++
		}
		if err := p.Store.Advance(batch.Next); err != nil {
			return res, err
		}
		res.Cursor = batch.Next
		if len(batch.Events) == 0 || batch.Next == cursor {
			res.CaughtUp, res.Partial = true, false
			return res, nil
		}
		cursor = batch.Next
	}
	return res, nil
}

// redactErr masks declared secrets out of err while preserving its typed code.
func redactErr(err error, target string, secrets []string) error {
	var ge *gws.Error
	if errors.As(err, &ge) {
		out := *ge
		out.Message = gws.RedactText(err.Error(), secrets...)
		if out.Target == "" {
			out.Target = target
		}
		return &out
	}
	return gws.NewError(gws.CodeProviderFailure, gws.RedactText(err.Error(), secrets...), nil).WithOp("poll", target)
}
