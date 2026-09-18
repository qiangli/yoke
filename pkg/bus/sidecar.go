package bus

import (
	"context"
	"fmt"
	"time"

	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/spacetime"
)

// Deliverer performs an INTERRUPT — breaking into a running turn.
//
// An interface, not a direct call, for two reasons. It is the only part of the
// sidecar that touches a live agent's terminal, so tests need to observe the
// decision without a pty; and it is the part most likely to grow a second
// implementation (an MCP notification, an A2A webhook) once the bus reaches past
// this host.
//
// Queued delivery is deliberately NOT here: it is just an append to the pending
// buffer, needs nothing live, and must keep working when no agent is running.
type Deliverer interface {
	Interrupt(sub Subscription, p Pending) error
}

// ctlSockDeliverer steers a live agent over the control socket agentpty serves —
// the same channel the coach uses.
type ctlSockDeliverer struct{}

// Interrupt sends ESC, then the notification text.
//
// The ESC is what makes this an interrupt rather than a queued line, and the
// order is load-bearing: a message alone is read only BETWEEN turns, so an agent
// stuck in a tool loop would never see it — that turn is not going to end on its
// own. ESC breaks the loop; the text that follows is then read as the next input.
func (ctlSockDeliverer) Interrupt(sub Subscription, p Pending) error {
	if sub.Instance == "" {
		return fmt.Errorf("no instance to steer")
	}
	card, ok, err := room.Find(sub.Instance)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("instance %q is not live", sub.Instance)
	}
	if card.CtlSock == "" {
		return fmt.Errorf("instance %q has no control socket", card.ID)
	}
	if err := agentpty.SendFrame(card.CtlSock, agentpty.VerbatimFrame([]byte{0x1b})); err != nil {
		return fmt.Errorf("interrupting %s: %w", card.ID, err)
	}
	text := fmt.Sprintf("[bus] %s from %s: %s", p.Topic, p.Principal, p.Body)
	if err := agentctl.Say(card.CtlSock, text); err != nil {
		return fmt.Errorf("steering %s: %w", card.ID, err)
	}
	_ = room.Emit(room.Event{Type: room.EventInterrupt, Actor: "bus", Target: card.ID})
	return nil
}

// Sidecar holds subscriptions on agents' behalf and resolves relevance and
// urgency off their critical path.
//
// The premise, from the design: a running agent is always heads-down in a turn,
// blocked between turns, or idle — and in none of those states can it reliably
// decide to go check a channel. Telling it to remember to look is the worst
// option, because it forgets or it is mid-task. So nothing is asked of the
// agent's cognition: this process watches continuously, and the agent reads a
// buffer that is already correct.
type Sidecar struct {
	Poll      time.Duration
	Deliverer Deliverer
	// Now is injectable so rate-limit behaviour is testable without sleeping.
	Now func() time.Time

	// sent tracks interrupt timestamps per subscriber for the rate limit.
	sent map[string][]time.Time
}

// NewSidecar returns a sidecar with the real control-socket deliverer.
func NewSidecar(poll time.Duration) *Sidecar {
	if poll <= 0 {
		poll = defaultPoll
	}
	return &Sidecar{Poll: poll, Deliverer: ctlSockDeliverer{}, Now: time.Now}
}

func (s *Sidecar) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Run polls until the context is cancelled.
func (s *Sidecar) Run(ctx context.Context) error {
	coordinate := spacetime.DefaultProbes(spacetime.NopCache())
	coordinate.StartMovementMonitor()
	defer coordinate.Close()

	// Once is a full parse of the host timeline for every subscription. Behind
	// the same gate as the chat relay: metadata every tick, the parse only when
	// the timeline or the subscription set moved, or on the periodic rescan —
	// otherwise a steward's sidecar is the second core-burner on an idle host
	// (coreutils story #127).
	gate := NewPollGate(TimelineFingerprint, 0)
	t := time.NewTicker(s.Poll)
	defer t.Stop()
	for {
		if read, sum, ok := gate.Due(s.now()); read {
			if _, err := s.Once(); err != nil {
				return err
			}
			gate.Commit(sum, ok, s.now())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Result reports what one pass did, so `--once` and the tests can assert on it
// instead of inferring from side effects.
type Result struct {
	Queued     int `json:"queued"`
	Interrupts int `json:"interrupts"`
	Demoted    int `json:"demoted"`
}

// Once performs a single pass: read the timeline, and for every subscription
// deliver what it has not yet been given.
//
// Ordering rule, the same one the drain path uses: the subscription's offset is
// advanced only AFTER the pending buffer has been written. Advancing first and
// then failing to append would consume a notification the agent never learns
// existed — a silent drop, which leaves it acting on stale assumptions.
func (s *Sidecar) Once() (Result, error) {
	var res Result

	subs, err := Subscriptions()
	if err != nil {
		return res, err
	}
	if len(subs) == 0 {
		return res, nil
	}
	events, err := room.Timeline(0)
	if err != nil {
		return res, err
	}

	for _, sub := range subs {
		var high int64
		changed := false
		for _, e := range events {
			if e.Seq > high {
				high = e.Seq
			}
			if e.Seq <= sub.Since || !sub.Matches(e) {
				continue
			}

			p := Pending{
				SchemaVersion: SchemaVersion,
				Seq:           e.Seq, TS: e.TS,
				Principal: e.Principal, Topic: e.Topic, To: e.To, Room: e.Room,
				Body:     e.Body,
				Delivery: DeliveryQueued,
			}

			// Resolve the delivery tier COMPLETELY — including actually attempting
			// the interrupt — before writing anything down.
			//
			// The append used to come first, which meant a failed interrupt could
			// only be recorded by mutating a struct already on disk: the buffer
			// kept `delivery: interrupt` for a delivery that never happened, and
			// the only trace of the failure was a counter. That is a record
			// asserting success because nothing recorded the failure — the exact
			// shape this codebase refuses. Decide first, then write once, and the
			// entry cannot disagree with what occurred.
			if e.Priority == DeliveryInterrupt {
				if reason := s.interruptRefusal(sub, e); reason != "" {
					// DEMOTE, never drop. Governance and rate limiting reduce
					// URGENCY; they do not decide whether the agent gets to know.
					// A notification that vanishes leaves an agent on stale
					// assumptions, and it cannot ask for what it never saw.
					p.Demoted = reason
					res.Demoted++
				} else if derr := s.Deliverer.Interrupt(sub, p); derr != nil {
					// A failed interrupt costs urgency, not the message.
					p.Demoted = "interrupt failed: " + derr.Error()
					res.Demoted++
				} else {
					p.Delivery = DeliveryInterrupt
					s.record(sub.Subscriber)
					res.Interrupts++
				}
			}
			if p.Delivery == DeliveryQueued {
				res.Queued++
			}

			// The offset advances only after this succeeds (below), so an append
			// failure re-delivers rather than silently consuming. A duplicate is
			// recoverable; a loss is not.
			if err := AppendPending(sub.Subscriber, p); err != nil {
				return res, err
			}
			changed = true
		}

		if changed || high > sub.Since {
			sub.Since = high
			if err := SaveSubscription(sub); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// interruptRefusal returns why an interrupt must be demoted, or "" to allow it.
func (s *Sidecar) interruptRefusal(sub Subscription, e room.Event) string {
	if !sub.AcceptsInterruptFrom(e.Principal) {
		// Default-closed: the power to break into an agent's turn is granted by
		// name, never assumed. Everyone may report; only named principals may
		// redirect.
		return fmt.Sprintf("principal %q is not in interrupt_from", e.Principal)
	}
	if sub.Instance == "" {
		return "no live instance to steer"
	}
	if n := s.rateUsed(sub.Subscriber); n >= sub.maxPerMin() {
		return fmt.Sprintf("rate limit reached (%d/min)", sub.maxPerMin())
	}
	return ""
}

// rateUsed counts interrupts delivered to a subscriber in the last minute.
func (s *Sidecar) rateUsed(subscriber string) int {
	cutoff := s.now().Add(-time.Minute)
	var live []time.Time
	for _, t := range s.sent[subscriber] {
		if t.After(cutoff) {
			live = append(live, t)
		}
	}
	if s.sent == nil {
		s.sent = map[string][]time.Time{}
	}
	s.sent[subscriber] = live
	return len(live)
}

func (s *Sidecar) record(subscriber string) {
	if s.sent == nil {
		s.sent = map[string][]time.Time{}
	}
	s.sent[subscriber] = append(s.sent[subscriber], s.now())
}
