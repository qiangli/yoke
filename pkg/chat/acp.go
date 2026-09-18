package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/acp"
	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/room"
)

// A REAL END OF TURN, from the tool, over a protocol.
//
// This file is rungs 1 and 2 of the ladder in pkg/agentlaunch/rung.go. Rung 4 —
// everything else in this package — decides a turn has ended by watching for 25
// seconds of SILENCE (Session.WaitIdle), which is wrong for an agent that pauses
// to think and wrong for one that renders a spinner, and costs the full 25
// seconds on the way out of every single turn. Rung 3 (events.go) fixed that for
// exactly one tool, over a side-channel file it has to be told to write.
//
// ACP fixes it over a protocol: a prompt is a REQUEST, and its response carries
// a structured StopReason. The turn is over when the agent says it is over. No
// timer, no scrape, no guess — and no pty either, so the tool's chrome never
// lands in the transcript in the first place.
//
// # What is deliberately unchanged
//
// Everything. A tool that declares no acp_exec never reaches this file, and the
// gate below means that even one that does is left alone until an operator opts
// in. Start's ACP branch is a single call at the top that reports "not mine"
// and falls through to the identical code that ran before ACP existed. The
// retrofit degrades, it never drops.

// acpDriver holds the ACP client behind a Session. Its presence on a Session is
// what makes the session an ACP session; nil means the pty transport, which is
// every session there is today.
type acpDriver struct {
	client  *acp.Client
	session string
	ctx     context.Context
	sink    io.Writer
	launch  Launch

	// end carries the completed turn. Buffered by one and drained before each
	// prompt, so a WaitIdle can never be satisfied by the PREVIOUS turn's report
	// — the exact failure the silence heuristic has, reintroduced by sloppy
	// signalling, would be a poor trade for a real protocol.
	end chan acpTurn

	mu     sync.Mutex
	turn   strings.Builder // the current turn's streamed message chunks
	active bool            // exactly one ACP prompt may be in flight

	closeOnce sync.Once
}

// acpTurn is one completed prompt: what the agent said, and why it stopped.
type acpTurn struct {
	text   string
	reason acp.StopReason
	err    error
}

// startACPSession is Start's ACP branch.
//
// It reports ok=false — with no error and NOTHING done — whenever this launch
// does not belong on an ACP rung, so the caller falls through to the path it
// took before ACP existed. That "nothing done" is load-bearing: no governance,
// no budget consumed, no process spawned, no room card. A leak here would spend
// a turn's budget twice on every non-ACP launch in the fleet.
func startACPSession(ctx context.Context, agent string, opt SessionOptions) (*Session, bool, error) {
	if !agentlaunch.ACPEnabled() {
		return nil, false, nil
	}
	name, err := ResolveAgent(agent, "")
	if err != nil {
		return nil, false, nil // the ordinary path reports this identically
	}

	// The ACP launch is its OWN template, so resolve with ACP and without Steer.
	//
	// Both halves matter. A tool can speak ACP and have no interactive pty launch
	// at all, and demanding a steer_exec it does not need would refuse a session
	// we can actually drive. And a tool can speak ACP and have no HEADLESS launch
	// either — an ACP-only tool declares its {model} slot in acp_exec, so a
	// resolution that reads the headless template would report that the tool
	// "cannot select a model" and throw the binding's model away.
	//
	// Everything else about the resolution — the containerized guard, read-only
	// argv stripping, the kill-switch guard, the credential firewall's
	// PreserveEnv — is the same call, on the same options, the pty path makes.
	// An ACP transport is not a licence to hand an agent unattended full access.
	lo := opt.launchOptions()
	lo.Steer = false
	lo.ACP = true
	l, err := resolveLaunch(name, lo)
	if err != nil {
		// Not an ACP launch (no acp_exec, unknown tool, a model this tool cannot
		// take): report "not mine" and let the caller fall to the rung below,
		// exactly as it did before ACP existed.
		return nil, false, nil
	}
	tool, known := newCatalog().Tool(l.ToolName)
	if !known || !agentlaunch.EffectiveRung(tool.CLI.Launch).IsACP() {
		return nil, false, nil
	}
	var preparedInbox bus.PreparedPreamble
	if strings.TrimSpace(opt.Prompt) != "" {
		preparedInbox = bus.PrepareForAgent(name, opt.Prompt)
		if err := preparedInbox.Err(); err != nil {
			return nil, true, fmt.Errorf("chat: prepare opening inbox: %w", err)
		}
		opt.Prompt = preparedInbox.Text
	}

	// From here we are committed, and errors are real errors: falling through to
	// the pty path after governing would bill the turn twice.
	next, d, err := governLaunch(ctx, name, l, opt.Prompt, lo)
	if err != nil {
		return nil, true, err
	}
	if d.Action == llmbudget.Queue {
		return nil, true, fmt.Errorf("chat: LLM budget queued %s for %s: %s", d.Delay, l.Binding(), d.Reason)
	}
	if d.Action == llmbudget.Block {
		return nil, true, fmt.Errorf("chat: LLM budget blocked %s: %s", l.Binding(), d.Reason)
	}
	l = next

	cwd := opt.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	s := &Session{
		Agent:        l.Binding(),
		Nick:         l.Nick,
		inboxAgent:   name,
		launch:       l,
		allowPremium: opt.AllowPremium,
		done:         make(chan struct{}),
		lastWrite:    time.Now(),
	}
	sink := io.Writer(&sessionWriter{s: s})
	if opt.Stream != nil {
		sink = io.MultiWriter(sink, opt.Stream)
	}
	drv := &acpDriver{ctx: ctx, sink: sink, launch: l, end: make(chan acpTurn, 1)}

	// Same trust preseed as the pty path: an ACP agent that stops to ask about
	// trust on stdio would deadlock the protocol rather than merely stall a TUI.
	cmd := agentCommand(withLaunch(ctx, l), l.Tool, l.Args, cwd)
	if p, ok := agentctl.ProfileFor(l.ToolName); ok && p.Preseed != "" {
		_ = agentctl.ApplyTrustPreseed(cmd.Dir, p.Preseed)
	}

	setProcessGroup(cmd)
	s.budgetCommand = cmd
	budgetWork, budgetErr := reserveBudgetWork(ctx, l, opt.Prompt, s.Agent, true, opt.AllowPremium)
	if budgetErr != nil {
		return nil, true, budgetErr
	}
	s.budgetWorks = append(s.budgetWorks, budgetWork)
	client, err := acp.NewClient(ctx, &acpHandler{d: drv}, cmd)
	if err != nil {
		if cmd.Process == nil {
			_ = budgetWork.abort()
		} else {
			_ = budgetWork.finish("", err)
		}
		return nil, true, fmt.Errorf("chat: %s speaks ACP but would not start: %w", l.Binding(), err)
	}
	drv.client = client
	sid, err := client.NewSession(ctx, cwd)
	if err != nil {
		_ = client.Close()
		_ = budgetWork.finish("", err)
		return nil, true, fmt.Errorf("chat: %s would not open an ACP session: %w", l.Binding(), err)
	}
	drv.session = sid
	s.acp = drv

	// Join the host room like any other member. CtlSock is empty: an ACP session
	// is steered through the protocol, not through a pty control socket, and
	// advertising a socket nobody is listening on would be worse than none.
	mode := strings.TrimSpace(opt.Mode)
	if mode == "" {
		mode = "session"
	}
	card := room.Card{
		ID:        agentID(l),
		Principal: principalName(),
		Tool:      l.ToolName,
		Model:     l.ModelName,
		Binding:   l.Binding(),
		Nick:      l.Nick,
		Band:      bindingBand(name),
		Mode:      mode,
		Task:      strings.TrimSpace(opt.Task),
		PID:       os.Getpid(),
		Cwd:       cwd,
		Events:    true, // it reports its own turn boundaries — the best kind
		Caps:      []string{room.CapInboxDelivery},
	}
	_ = room.Join(card)
	s.acpCard = card.ID

	// WATCH FOR A DEATH NOBODY ASKED FOR.
	//
	// closeACP is the caller's path out, so before this every route to
	// close(s.done) began with the caller deciding to leave. An agent that
	// crashed — or was OOM-killed, or exited on its own — closed nothing: the
	// protocol just went quiet, Live() kept answering true, and anything
	// waiting on the session waited forever. The pty path never had this hole
	// because a dead child closes the pty and the reader returns EOF; ACP has
	// no equivalent symptom, which is exactly why it needs an explicit watcher.
	//
	// Started AFTER the room join so a death cannot race past the card and skip
	// room.Leave, which would strand a member card for a process that is gone.
	// closeACP is sync.Once-guarded, so the ordinary caller path and this one
	// cannot double-close.
	if dead := client.Done(); dead != nil {
		go func() {
			<-dead
			s.closeACP()
		}()
	}

	// The opening prompt needs no waitReady and no re-send ladder. Those exist
	// because a TUI silently swallows keystrokes it is not ready for; a JSON-RPC
	// request is either answered or it is an error.
	if strings.TrimSpace(opt.Prompt) != "" {
		if err := drv.prompt(opt.Prompt); err != nil {
			s.closeACP()
			return s, true, err
		}
	}
	recordPreambleAdmission(ctx, preparedInbox)
	if err := preparedInbox.Commit(); err != nil {
		s.closeACP()
		return s, true, fmt.Errorf("chat: opening ACP prompt was delivered but its inbox acknowledgement failed: %w", err)
	}
	s.startInboxRelay(ctx)
	return s, true, nil
}

// prompt starts a turn. It does not block: Start and Say both return as soon as
// the agent has been asked, and WaitIdle is where a caller waits for the answer
// — the same shape the pty path has, so callers do not change.
func (d *acpDriver) prompt(text string) error {
	d.mu.Lock()
	if d.active {
		d.mu.Unlock()
		return fmt.Errorf("chat: ACP turn is still active")
	}
	d.active = true
	d.mu.Unlock()
	// Drop any unread report from a previous turn: this turn's boundary is the
	// only one that can end this turn.
	select {
	case <-d.end:
	default:
	}
	d.mu.Lock()
	d.turn.Reset()
	d.mu.Unlock()

	go func() {
		callCtx, endObservation := startGenAIObservation(d.ctx, d.launch)
		reason, err := d.client.Prompt(callCtx, d.session, text)
		d.mu.Lock()
		said := d.turn.String()
		d.turn.Reset()
		d.active = false
		d.mu.Unlock()
		select {
		case d.end <- acpTurn{text: said, reason: reason, err: err}:
		default:
		}
		endGenAIObservation(endObservation, d.launch, text, said, string(reason), err)
	}()
	return nil
}

func (d *acpDriver) idle() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Preserve the completed turn for the caller's WaitIdle. Starting the inbox
	// turn before that result is consumed would make prompt() drain it as stale.
	return !d.active && len(d.end) == 0
}

// waitACPTurn is WaitIdle for an ACP session: it waits for the agent to SAY the
// turn ended. No ticker, no quiet period, no 25-second exit tax.
func (s *Session) waitACPTurn(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case t := <-s.acp.end:
		s.mu.Lock()
		// The answer arrives as DATA. Turn() prefers it over the buffer for the
		// same reason the events path does: nothing here was scraped.
		s.reported = t.text
		s.acpStop = t.reason
		s.mu.Unlock()
		if t.err != nil {
			return fmt.Errorf("chat: %s: ACP turn failed: %w", s.Nick, t.err)
		}
		return nil
	}
}

// ACPStopReason is why the agent stopped its last completed turn ("end_turn",
// "refusal", "cancelled", …), or "" for a session that is not on an ACP rung.
//
// This is the whole point of the retrofit, exposed: rung 4 cannot answer this
// question at all, because silence has no reason attached to it.
func (s *Session) ACPStopReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.acpStop)
}

// Rung reports which transport this session is being driven on.
func (s *Session) Rung() agentlaunch.Rung {
	if s.acp != nil {
		// Which of the two ACP rungs is a property of the tool's declaration.
		if tool, ok := newCatalog().Tool(s.launch.ToolName); ok {
			return agentlaunch.RungFor(tool.CLI.Launch)
		}
		return agentlaunch.RungACPNative
	}
	if s.events != nil {
		return agentlaunch.RungEvents
	}
	return agentlaunch.RungPTY
}

// closeACP ends the ACP session: cancel anything in flight, reap the
// subprocess, record what the conversation spent, and leave the room.
func (s *Session) closeACP() {
	s.acp.closeOnce.Do(func() {
		_ = s.acp.client.Cancel(context.Background(), s.acp.session)
		_ = s.acp.client.Close()
		s.mu.Lock()
		spoke := s.buf.String()
		s.mu.Unlock()
		s.mu.Lock()
		s.err = errors.Join(s.err, s.finishBudgetWorks(spoke, budgetCompletionError(s.budgetCommand)))
		s.mu.Unlock()
		if s.acpCard != "" {
			room.Leave(s.acpCard)
		}
		close(s.done)
	})
}

// acpHandler bridges ACP session updates into the ONE thing a Session does with
// streamed output: write it to the sink, which is the same sessionWriter (plus
// the caller's optional tee) the pty path writes through. Callers that watch a
// session see an ACP agent exactly as they see a scraped one.
type acpHandler struct {
	acp.BaseHandler
	d *acpDriver
}

func (h *acpHandler) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		text := u.AgentMessageChunk.Text
		if text == "" {
			return nil
		}
		h.d.mu.Lock()
		h.d.turn.WriteString(text)
		h.d.mu.Unlock()
		_, _ = io.WriteString(h.d.sink, text)
	case u.ToolCall != nil, u.ToolCallUpdate != nil:
		// A tool call as STRUCTURED DATA — the other half of the prize, and what
		// the fleet-evidence rule has been asking for. Nothing consumes it yet;
		// it deliberately does NOT go into the transcript, because inventing
		// chrome the agent did not print would corrupt the very thing ACP is
		// supposed to give us: a clean answer.
	}
	// Thought chunks are dropped: reasoning is not the answer, and rung 4 never
	// saw it either.
	return nil
}
