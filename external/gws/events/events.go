// Package events provides durable polling, push-receiver and subscription
// lease adapters for the Google Workspace CLI integration (Sprint 457 story
// 5548854717bc): Gmail history polling, Gmail watch renewal, and Workspace
// Events subscription renewal, plus an authenticated Pub/Sub push handler.
//
// Everything here is OPT-IN. Nothing persists unless the caller opens a
// Store on an explicit directory, nothing listens unless the caller mounts
// Receiver on a server it owns (and hosting one is a managed service gated
// by ServeAdmission), and nothing renews unless the caller schedules it.
//
// Delivery guarantee: AT-LEAST-ONCE, never exactly-once. The processing
// boundary is the Handler: an event is acknowledged (recorded in the
// checkpoint's dedup window) only after its Handler returned nil, and the
// change cursor advances only after every event of the batch it closes was
// acknowledged, in provider order. A crash between the Handler's downstream
// action and the acknowledgment redelivers that event on restart, so a
// Handler must be idempotent on Event.ID (the idempotency key). Fetching a
// page is not acknowledgment; pagination (provider page tokens, walked
// inside one Source.Poll) is distinct from the change cursor stored here.
//
// Reused machinery, no competing schema: typed failures are gws.Error on
// the shared loom-v2 exit mapping; cursor tokens stay opaque and bound to a
// gws.Binding (the concrete bashy-cursor-v1 serialization is owned by 446
// S19); single-consumer claims use coreutils pkg/lockfile; periodic renewal
// rides the existing scheduler (pkg/schedule jobs over the registered gws
// command). The checkpoint file is event-adapter state only — it is NOT a
// general database and does not replace Sprint 453 storage APIs.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/qiangli/yoke/external/gws"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// Event is one change notification, in provider order.
type Event struct {
	// ID is the provider's stable identity for the change (Gmail history id,
	// CloudEvents ce-id, Pub/Sub messageId). It is the dedup and idempotency key.
	ID string `json:"id"`
	// Kind is the provider event type when known (e.g. a CloudEvents ce-type).
	Kind string `json:"kind,omitempty"`
	// Payload is the provider record, carried verbatim.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Batch is one change-stream step: the events after a cursor and the cursor
// that follows them. Next is opaque; it is never parsed by consumers.
type Batch struct {
	Events []Event
	Next   string
}

// Source fetches the changes after cursor. A Source walks provider
// pagination internally (bounded) and returns the change cursor that closes
// the batch. An expired cursor is a typed gws.CodeCursorExpired error.
type Source interface {
	Poll(ctx context.Context, cursor string) (Batch, error)
}

// Handler performs the downstream processing of one event. Returning nil
// is the processing boundary: the event is then acknowledged. Downstream
// actions need their own authorization; this package grants none.
type Handler func(ctx context.Context, ev Event) error

// Runner executes one bounded registered-command invocation (argv[0] is the
// command, e.g. "gws") and returns its stdout. A non-nil error carries the
// failure; stdout may still hold the provider's error body.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// MaxRunOutput bounds the stdout read from one gws invocation.
const MaxRunOutput = 8 << 20

// GwsRunner runs argv through bin, the gws executable the caller resolved
// from the existing registry entry (registry.Lookup("gws").Ensure — host-first,
// no silent download), with tracking disabled and bounded stdout. argv[0]
// must be "gws": this runner never executes an arbitrary PATH command.
func GwsRunner(bin string) Runner {
	return func(ctx context.Context, argv []string) ([]byte, error) {
		if bin == "" || len(argv) == 0 || argv[0] != "gws" {
			return nil, gws.NewError(gws.CodeInvalidInput, "gws events: runner executes only a resolved registered gws command", nil)
		}
		c := binmgr.Command(ctx, bin, argv[1:]...)
		c.Env = gws.ChildEnv(os.Environ())
		var stderr bytes.Buffer
		c.Stderr = &limitedWriter{w: &stderr, n: 64 << 10}
		stdout, err := c.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := c.Start(); err != nil {
			return nil, err
		}
		out, berr := gws.ReadBounded(stdout, MaxRunOutput)
		if berr != nil {
			_ = c.Process.Kill()
			_ = c.Wait()
			return nil, berr
		}
		if werr := c.Wait(); werr != nil {
			if len(out) == 0 {
				out = stderr.Bytes()
			}
			return out, werr
		}
		return out, nil
	}
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, err := l.w.Write(q)
	return len(p), err
}
