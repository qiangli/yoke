package weave

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmbudget"
)

type renewHarness struct {
	mu        sync.Mutex
	now       time.Time
	cancelled bool
	log       bytes.Buffer
}

func (h *renewHarness) clock() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }

// drive feeds one tick per result; each renew call advances the fake clock 30s.
func (h *renewHarness) drive(t *testing.T, results []error) {
	t.Helper()
	h.now = time.Unix(1000, 0)
	i := 0
	l := weaveRenewLoop{
		renew: func(context.Context) error {
			h.mu.Lock()
			h.now = h.now.Add(weaveRenewInterval)
			h.mu.Unlock()
			e := results[i]
			i++
			return e
		},
		now: h.clock,
		ttl: weaveReservationTTL,
		log: &h.log,
		run: "r1",
	}
	tick := make(chan time.Time)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.serve(ctx, tick, func() { h.cancelled = true; stop() })
	}()
	for range results {
		select {
		case tick <- time.Time{}:
		case <-done:
			<-done
			return
		}
	}
	stop()
	<-done
}

func TestWeaveRenewTransientFailureKeepsRunAlive(t *testing.T) {
	h := &renewHarness{}
	h.drive(t, []error{errors.New("llmbudget: meter busy"), nil, errors.New("disk full"), nil})
	if h.cancelled {
		t.Fatalf("transient renew failure cancelled the run; log=%q", h.log.String())
	}
	if !strings.Contains(h.log.String(), "meter busy") || !strings.Contains(h.log.String(), "will retry") {
		t.Fatalf("log missing error/retry: %q", h.log.String())
	}
}

func TestWeaveRenewDroughtPastTTLMarginCancels(t *testing.T) {
	h := &renewHarness{}
	e := errors.New("meter lock wait timeout")
	h.drive(t, []error{e, e, e, e})
	if !h.cancelled {
		t.Fatal("sustained failure past ttl margin did not cancel")
	}
	if !strings.Contains(h.log.String(), "meter lock wait timeout") || !strings.Contains(h.log.String(), "cancelling") {
		t.Fatalf("log: %q", h.log.String())
	}
}

func TestWeaveRenewDefiniteLossCancelsImmediately(t *testing.T) {
	for _, e := range []error{llmbudget.ErrReservationMissing, llmbudget.ErrOwnerNotLive} {
		h := &renewHarness{}
		h.drive(t, []error{e, nil})
		if !h.cancelled || !strings.Contains(h.log.String(), e.Error()) {
			t.Fatalf("%v: cancelled=%v log=%q", e, h.cancelled, h.log.String())
		}
	}
}
