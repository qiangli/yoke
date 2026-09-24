package chat

import (
	"context"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
)

// managedInboxPoll is deliberately modest: input remains durable in its source,
// and a one-second wake-up is fast compared with an agent turn without turning
// every managed session into a filesystem hot loop.
//
// The interval alone did not keep it from becoming one. A tick's body is a full
// unified-inbox snapshot — two parses of the host timeline plus the host's
// board and meet scan — which on a mature host takes longer than the tick, so
// the ticker fired back-to-back and an IDLE managed session burned a whole
// core (coreutils story #127: `foreman serve` at 66–126% while its agent sat at
// 2%). The snapshot therefore runs behind a bus.PollGate: cheap store metadata
// every tick, the full read only when something moved or on the periodic
// rescan. Same mechanism as the Sprint 138 fix to `bashy inbox --watch`.
const managedInboxPoll = time.Second

// runInboxRelay turns durable unified-inbox input into an actual agent turn.
// prepare MUST be non-consuming; deliver owns acknowledgement and may refuse
// while a transport is busy. A refusal leaves every cursor untouched and the
// next poll retries the same input.
func runInboxRelay(ctx context.Context, done <-chan struct{}, ready func() bool,
	prepare func() bus.PreparedPreamble, deliver func(bus.PreparedPreamble) error, gate *bus.PollGate) {
	runInboxRelayEvery(ctx, done, ready, prepare, deliver, gate, managedInboxPoll)
}

// runInboxRelayEvery is runInboxRelay with an injectable interval. A nil gate
// snapshots on every tick — the tests' shape, and the pre-#127 behaviour.
func runInboxRelayEvery(ctx context.Context, done <-chan struct{}, ready func() bool,
	prepare func() bus.PreparedPreamble, deliver func(bus.PreparedPreamble) error,
	gate *bus.PollGate, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-tick.C:
		}
		// Readiness first: it is a field load, and a busy transport must not
		// even sample the stores — the answer would be thrown away.
		if ready != nil && !ready() {
			continue
		}
		var sum uint64
		var ok bool
		if gate != nil {
			var read bool
			if read, sum, ok = gate.Due(time.Now()); !read {
				continue
			}
		}
		pending := prepare()
		if pending.Err() != nil || pending.Text == "" {
			if gate != nil {
				gate.Commit(sum, ok, time.Now())
			}
			continue
		}
		if err := deliver(pending); err != nil {
			// Failure is durable: no Commit means retry — and the gate must not
			// hide that retry behind an unchanged fingerprint.
			if gate != nil {
				gate.Retry()
			}
			continue
		}
		if gate != nil {
			gate.Commit(sum, ok, time.Now())
		}
	}
}

func (s *Session) startInboxRelay(ctx context.Context) {
	ready := func() bool { return true }
	if s.acp != nil {
		ready = s.acp.idle
	}
	go runInboxRelay(ctx, s.done, ready,
		func() bus.PreparedPreamble { return bus.PrepareForAgent(s.inboxAgent, "") },
		s.deliverPreparedInbox, bus.NewInboxPollGate(s.inboxAgent))
}

// deliverPreparedInbox is Say with the already-prepared input preserved. This
// avoids a second snapshot (and duplicate rendering), while retaining the same
// budget gate, transport, metering, and commit-after-delivery contract as Say.
func (s *Session) deliverPreparedInbox(p bus.PreparedPreamble) error {
	if err := p.Err(); err != nil {
		return err
	}
	if p.Text == "" {
		return nil
	}
	if d := s.governTurn(p.Text); !d.Allowed() {
		return nil
	}
	if s.acp == nil && s.ptyInbox != nil {
		// Typed into a TUI: bounded, and acknowledged only when it went in whole.
		complete, err := s.ptyInbox.deliver(p.Text, s.say)
		if err != nil || !complete {
			return err
		}
		recordPreambleAdmission(context.Background(), p)
		if err := p.Commit(); err != nil {
			return err
		}
		recordLaunchUsageTokens(context.Background(), s.launch, estimateTokens(p.Text), 0)
		return nil
	}
	if err := s.say(p.Text); err != nil {
		return err
	}
	recordPreambleAdmission(context.Background(), p)
	if err := p.Commit(); err != nil {
		return err
	}
	recordLaunchUsageTokens(context.Background(), s.launch, estimateTokens(p.Text), 0)
	// The relay owns this unsolicited ACP turn, so it also consumes that turn's
	// completion. Otherwise the completed result would keep the transport
	// non-idle forever and only the first inbox message could be delivered.
	if s.acp != nil {
		return s.waitACPTurn(s.acp.ctx)
	}
	return nil
}
