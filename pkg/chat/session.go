package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	whodb "github.com/qiangli/coreutils/pkg/who"
	"github.com/qiangli/yoke/pkg/acp"
	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/room"
)

// A Session is a live agent you can talk to.
//
// It exists because the fleet had grown THREE different ideas of what "steering"
// means, and only one of them steered anything:
//
//   - `weave say`   wrote to a run's pty control socket. Real mid-turn steering.
//   - `meet say`    wrote to the same kind of socket — but meet's turns were
//     headless one-shots, so there was usually nothing on the
//     other end to hear it.
//   - `foreman tell` queued a message and spawned a WHOLE NEW chat.Invoke. That is
//     conversation, not steering: it cannot interrupt anything,
//     because by the time the message lands the agent has exited.
//
// One primitive, so a chair, a conductor, a foreman and an operator all mean the
// same thing by "tell it to stop doing that".
//
// # Session vs Invoke
//
// Invoke is a QUESTION: one prompt, one answer, a clean captured turn (stdout and
// stderr stay apart on a pipe). Use it when you want a reply.
//
// Session is a CONVERSATION: the agent's interactive launch (steer_exec) under a
// pty, held open, with a control socket. Use it when you need to interrupt.
//
// The trade is real and the registry refuses to hide it: a pty merges stdout and
// stderr, so the tool's own chrome lands in the captured text. You pay that to be
// able to say "stop, you're off the agenda" without killing the turn and losing
// everything it had already said.
type Session struct {
	budgetMu      sync.Mutex
	budgetWorks   []*budgetWork
	budgetClosed  bool
	budgetCommand *exec.Cmd
	Agent         string // the canonical binding, as recorded
	Nick          string // the name the caller used
	CtlSock       string // where a steer lands
	// inboxAgent is used only by transports such as ACP that have an
	// authenticated protocol session but no PTY control socket.
	inboxAgent string

	// events is the tool's structured channel, when it has one. Its presence is
	// the difference between KNOWING a turn ended and guessing from silence.
	events *eventTail

	// acp is the Agent Client Protocol transport, when the tool speaks it and an
	// operator has opted in (BASHY_ACP=1). Non-nil replaces the pty and the
	// silence heuristic with a real request/response carrying a StopReason. Nil
	// — which is every session the fleet runs today — leaves every path below
	// exactly as it was. See acp.go.
	acp     *acpDriver
	acpCard string // the host-room card an ACP session joined, left on close
	acpStop acp.StopReason

	// launch is the resolved, already-governed launch this session runs under. It
	// is retained because a session is not one LLM call — every steer that lands
	// on Say is ANOTHER billable turn against the same model, and the gate needs
	// the model name and lane to judge it.
	launch       Launch
	allowPremium bool

	mu         sync.Mutex
	buf        strings.Builder
	mark       int       // end of the last Turn
	reported   string    // the answer the tool ASSERTED on its event channel
	eventsOnce sync.Once // the silent-degradation warning fires once per session, not per turn
	lastWrite  time.Time
	// lastSteer is when we last SPOKE to the agent. WaitIdle measures silence from
	// whichever came later, this or lastWrite — see the comment there.
	lastSteer time.Time
	done      chan struct{}
	cancel    context.CancelFunc // force-stops the PTY after Close's graceful /quit window
	exit      int
	err       error
	killed    string
}

// SessionOptions configures a live agent session.
type SessionOptions struct {
	// Prompt opens the conversation. A tool whose steerable launch takes a prompt
	// on the command line (agy -i) gets it there; one that opens an empty session
	// (codex, opencode) is SENT it over the control channel once it is up, which
	// is the same thing from the caller's side and not from the tool's.
	Prompt string
	// OpeningSendOnce disables the generic TUI-startup resend loop for callers
	// whose protocol promises byte-exact, at-most-once delivery. The ordinary
	// chat path keeps its retry behavior; sprint-managed Foreman sessions opt in.
	OpeningSendOnce bool

	Cwd     string
	Timeout time.Duration

	// Stream receives the agent's output as it is written — the same tee Invoke
	// offers, so an observer sees a session exactly as it sees a turn.
	Stream io.Writer

	// ReadOnly strips the approval-gate kill-switches. A session that only has to
	// TALK needs no write authority, and read-only passes the launch guard by
	// construction on an ordinary host.
	ReadOnly bool

	// Attended marks a session a HUMAN is driving turn by turn through a proxying
	// front end — ycode's /agent attach is the case this exists for: the operator
	// stays in their own TUI, every message is theirs, and the agent's output is
	// rendered straight back to them.
	//
	// It is the same contract Interact carries for `bashy chat -i` (there the
	// human's terminal IS the session; here a front end relays it), and it means
	// the same thing to the launcher: strip the auto-approve kill-switches so the
	// tool's OWN approval gate stays on, keeping full write capability without
	// asking the uncontained-host guard to allow unattended full access.
	//
	// A session with no human on the other end must leave this false. Foreman,
	// meet and coach drive agents programmatically, so they do.
	Attended bool

	// AllowPremium explicitly bypasses LLM budget/subscription gates for urgent
	// human-authorized work. Usage is still recorded when the session produces
	// text.
	AllowPremium bool

	// AllowUnsafe keeps the agent CLI's approval-gate kill-switches, accepting
	// the uncontained-host risk — the session equivalent of `bashy chat --yolo`.
	//
	// It exists for the case Interact already had a flag for and Session did
	// not: a session nobody sits at, supervised REMOTELY through steer. With the
	// tool's own approval gate on and no terminal to answer it, such a session
	// does not fail — it STALLS, silently, looking alive. That is the worse
	// outcome, and the operator needs to be able to choose the other one.
	//
	// It is NOT implied by !Attended. An unattended session is the normal case
	// (foreman, meet, coach) and none of those wants this; the risk has to be
	// accepted explicitly or not at all.
	AllowUnsafe bool

	// Mode labels this instance on its host-room card (foreman | meet | coach | …).
	// Empty defaults to "session". It is how `bashy chat sessions` shows WHAT a
	// member is doing, not just that it exists.
	Mode string
	// Task identifies the owning work/session on the room card. Foreman uses its
	// durable session ID so callers can distinguish the intended manager from an
	// unrelated live session using the same agent identity.
	Task string
}

// launchOptions is the ONE translation from a session's options to the launcher's.
//
// Resolution and governance both take it, and they must be handed the identical
// value: resolveLaunch is what strips or refuses argv, governLaunch is what judges
// the same launch against budget. Building the two separately is how an attended
// session comes to be resolved as attended and governed as something else.
func (o SessionOptions) launchOptions() Options {
	return Options{
		Cwd:      o.Cwd,
		ReadOnly: o.ReadOnly,
		// ReadOnly is stricter and wins — same precedence as Interact.
		Attended:     o.Attended && !o.ReadOnly,
		AllowPremium: o.AllowPremium,
		// ReadOnly wins here too: it removes the dangerous flags rather than
		// asking to keep them, so "read-only and unsafe" is not a state that
		// should be reachable by setting two fields.
		AllowUnsafe: o.AllowUnsafe && !o.ReadOnly,
		Steer:       true, // the interactive launch, never the one-shot
	}
}

// Start launches an agent's interactive session.
//
// It fails loudly when the tool has no steerable launch, rather than quietly
// starting a one-shot that will exit before the first steer arrives. A caller
// that asked for a conversation must not be handed a monologue.
func Start(ctx context.Context, agent string, opt SessionOptions) (*Session, error) {
	// THE ACP RUNG, and the one line of Start that ACP added.
	//
	// It reports mine=false — having done nothing at all — for every launch that
	// is not on an ACP rung, which is every launch in the fleet until a tool
	// declares acp_exec AND an operator sets BASHY_ACP=1. Everything below this
	// point is byte-for-byte the function that ran before ACP existed, including
	// the pty requirement: an ACP agent needs no pty, so its branch must be
	// decided before a platform without one refuses the session.
	if s, mine, err := startACPSession(ctx, agent, opt); mine {
		return s, err
	}

	if !agentpty.Supported() {
		return nil, fmt.Errorf("chat: a steerable session needs a pty, which this platform has no support for")
	}

	name, err := ResolveAgent(agent, "")
	if err != nil {
		return nil, err
	}
	// Deliberately the SAME resolver Invoke uses. A steerable session differs from
	// a turn in exactly one respect — which launch template it renders — and
	// everything else (the containerized guard, the read-only argv stripping, the
	// canonical binding) must not be able to drift.
	lo := opt.launchOptions()
	l, err := resolveLaunch(name, lo)
	if err != nil {
		return nil, err
	}
	// UNREAD ON LOGIN. Hand the agent its mail as part of the prompt it starts
	// with, so a message sent while it was down is waiting when it comes up.
	//
	// Before governLaunch, deliberately — the same ordering Say uses, for the
	// same reason: the notification text is really sent, so it is really billed,
	// and metering the caller's prompt alone would understate the turn.
	//
	// Only when there IS a prompt. An interactive launch with no seed prompt has
	// an argv shape that depends on the prompt being absent (l.TakesPrompt gates
	// whether it is appended at all), and inventing one to carry mail would
	// change how the tool starts. Those sessions get their mail on the first
	// Say instead, which already prepends.
	var preparedInbox bus.PreparedPreamble
	if strings.TrimSpace(opt.Prompt) != "" {
		preparedInbox = bus.PrepareForAgent(name, opt.Prompt)
		if err := preparedInbox.Err(); err != nil {
			return nil, fmt.Errorf("chat: prepare opening inbox: %w", err)
		}
		opt.Prompt = preparedInbox.Text
	}
	next, d, err := governLaunch(ctx, name, l, opt.Prompt, lo)
	if err != nil {
		return nil, err
	}
	if d.Action == llmbudget.Queue {
		return nil, fmt.Errorf("chat: LLM budget queued %s for %s: %s", d.Delay, l.Binding(), d.Reason)
	}
	if d.Action == llmbudget.Block {
		return nil, fmt.Errorf("chat: LLM budget blocked %s: %s", l.Binding(), d.Reason)
	}
	l = next

	argv := append([]string{}, l.Args...)

	// If the tool can TELL us when a turn ends, let it. See pkg/chat/events.go:
	// everything else here infers a turn boundary from 25 seconds of silence,
	// which is wrong for an agent that pauses to think and wrong for one that
	// renders a spinner.
	id := agentID(l)

	var tail *eventTail
	if tl, ok := newCatalog().Tool(l.ToolName); ok && tl.ReportsTurnEnd() {
		evPath, err := sessionEventsPath(id)
		if err == nil {
			if extra := agentlaunch.EventFileArgsWithCatalog(toAgentLaunch(l), evPath, newCatalog); len(extra) > 0 {
				argv = append(argv, extra...)
				tail = &eventTail{path: evPath}
			}
		}
	}

	if l.TakesPrompt && strings.TrimSpace(opt.Prompt) != "" {
		argv = append(argv, opt.Prompt)
	}

	cwd := opt.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// The socket is short and hashed, NOT under a long workspace path: a unix
	// socket address caps at ~104 bytes, and blowing that degrades steering to a
	// polling file channel without saying so.
	sock, err := sessionSock(id)
	if err != nil {
		return nil, err
	}

	// The one-shot runner uses this same constructor. Session chooses steer_exec
	// argv and a PTY below; neither choice gets a separate environment policy.
	// Retain a child cancellation handle. Close first asks the TUI to leave, but
	// an agent is allowed to ignore /quit; after the grace window the session
	// contract still has to end the process tree rather than merely unlinking its
	// control socket and pretending it is gone.
	runCtx, cancel := context.WithCancel(ctx)
	cmd := agentCommand(withLaunch(runCtx, l), l.Tool, argv, cwd)

	// Prevention before cure: seed the tool's own config so its trust prompt never
	// appears. agentpty's gate classifier is the backstop for the ones it misses.
	if p, ok := agentctl.ProfileFor(l.ToolName); ok && p.Preseed != "" {
		_ = agentctl.ApplyTrustPreseed(cmd.Dir, p.Preseed)
	}

	s := &Session{
		Agent:        l.Binding(),
		Nick:         l.Nick,
		CtlSock:      sock,
		inboxAgent:   name,
		events:       tail,
		launch:       l,
		allowPremium: opt.AllowPremium,
		done:         make(chan struct{}),
		cancel:       cancel,
		// Seed the idle clock at launch. An agent that says NOTHING must still go
		// idle — otherwise WaitIdle would hang forever on the one failure it most
		// needs to report, which is an agent that never spoke.
		lastWrite: time.Now(),
	}

	sink := io.Writer(&sessionWriter{s: s})
	if opt.Stream != nil {
		sink = io.MultiWriter(sink, opt.Stream)
	}

	// Join the host room so a Session — the primitive foreman/meet/coach steer
	// through — is a discoverable, steerable member like any other instance. Left
	// when the run goroutine ends. (No LogPath: a Session captures in memory, so it
	// is steer/observe-able via the ctlsock, not `chat attach`.)
	mode := strings.TrimSpace(opt.Mode)
	if mode == "" {
		mode = "session"
	}
	card := room.Card{
		ID:        id,
		Principal: principalName(),
		Tool:      l.ToolName,
		Model:     l.ModelName,
		Binding:   l.Binding(),
		Nick:      l.Nick,
		Band:      bindingBand(name),
		Mode:      mode,
		Task:      strings.TrimSpace(opt.Task),
		CtlSock:   sock,
		PID:       os.Getpid(),
		Cwd:       cwd,
		Events:    tail != nil,
		Caps:      []string{room.CapInboxDelivery},
	}
	if tail != nil {
		card.EventsPath = tail.path
	}
	// CLAIM BEFORE SPAWNING. The claim is what enforces one live session per
	// identity, and it has to happen before the child exists — starting the
	// process first and discovering the conflict afterwards would leave a real
	// agent running with no card, unsteerable and invisible to `chat sessions`.
	if err := room.Join(card); err != nil {
		cancel()
		var live *room.ErrLive
		if errors.As(err, &live) {
			prior, _, _ := room.Find(live.ID)
			return nil, errAgentLive(live.ID, live.PID, prior.Cwd)
		}
		return nil, err
	}
	releaseInbox, err := bus.BindInstance(name, card.ID)
	if err != nil {
		cancel()
		room.Leave(card.ID)
		return nil, fmt.Errorf("chat: bind %s inbox to live session: %w", name, err)
	}
	// The socket name is stable for the agent identity. A crashed or just-stopped
	// predecessor can therefore leave a pathname behind. OnStart fires before
	// agentpty binds the new listener, so waitReady could otherwise mistake that
	// stale path for this session's channel and write into a dying listener.
	// Remove it only AFTER the singleton room claim succeeds; doing this before
	// the claim could disconnect a legitimately live session.
	if err := prepareSessionSocket(sock); err != nil {
		cancel()
		_ = releaseInbox()
		room.Leave(card.ID)
		return nil, err
	}

	budgetWork, budgetErr := reserveBudgetWork(ctx, l, opt.Prompt, card.ID, true, opt.AllowPremium)
	if budgetErr != nil {
		cancel()
		_ = releaseInbox()
		room.Leave(card.ID)
		return nil, budgetErr
	}
	s.budgetWorks = append(s.budgetWorks, budgetWork)
	started := make(chan error, 1)
	var startedOnce sync.Once
	reportStarted := func(err error) {
		startedOnce.Do(func() { started <- err })
	}

	go func() {
		defer cancel()
		defer close(s.done)
		defer room.Leave(card.ID)
		defer func() { _ = releaseInbox() }()
		var login *whodb.Handle
		var ptyLink string
		defer func() {
			_ = login.Close()
			if ptyLink != "" {
				_ = os.Remove(ptyLink)
			}
		}()
		exit, killed, err := agentpty.Run(cmd, sink, agentpty.Options{
			CtlSock:    sock,
			Capture:    true, // the caller records this; the human watches via observe
			MaxRuntime: opt.Timeout,
			OnStart: func(realTTY string) error {
				line, link, err := publishSessionTTY(id, realTTY)
				if err != nil {
					reportStarted(err)
					return err
				}
				h, err := whodb.Register(whodb.Record{
					Name:     id,
					TTY:      line,
					ID:       id,
					PID:      os.Getpid(),
					Started:  time.Now(),
					Surfaces: []string{"write", "mb", "bus"},
				})
				if err != nil {
					_ = os.Remove(link)
					reportStarted(err)
					return err
				}
				login = h
				ptyLink = link
				reportStarted(nil)
				return nil
			},
		})
		reportStarted(err)
		s.mu.Lock()
		s.exit, s.killed, s.err = exit, killed, err
		if cmd.Process == nil {
			_ = budgetWork.abort()
		}
		s.err = errors.Join(s.err, s.finishBudgetWorks(s.buf.String(), budgetCompletionError(cmd)))
		s.mu.Unlock()
	}()

	if err := <-started; err != nil {
		s.abortStart()
		return s, err
	}

	// A tool that opens an EMPTY session takes its first message over the wire.
	if !l.TakesPrompt && strings.TrimSpace(opt.Prompt) != "" {
		if err := s.waitReady(ctx); err != nil {
			s.abortStart()
			return s, err
		}
		if err := s.openConversation(ctx, opt.Prompt, opt.OpeningSendOnce); err != nil {
			s.abortStart()
			return s, err
		}
	}
	recordPreambleAdmission(ctx, preparedInbox)
	if err := preparedInbox.Commit(); err != nil {
		s.abortStart()
		return s, fmt.Errorf("chat: opening prompt was delivered but its inbox acknowledgement failed: %w", err)
	}
	s.startInboxRelay(ctx)
	return s, nil
}

func prepareSessionSocket(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("chat: clear stale control channel %s: %w", path, err)
}

// openConversation sends the first message and CONFIRMS it arrived.
//
// It does not send and hope. A TUI that has not finished starting silently
// swallows whatever you type at it, and the resulting session sits at an empty
// prompt forever — which looks, from every angle, like a model that had nothing to
// say. Observed exactly that: an opencode conductor idled at its splash screen with
// `calls=0`, and the obvious conclusion ("deepseek did nothing") would have been
// wrong, and would have been recorded as a fact about the model.
//
// So: send, then look for the agent to actually start writing. If it does not,
// send once more. Confirmation is cheap; a false verdict about a model is not.
func (s *Session) openConversation(ctx context.Context, prompt string, sendOnce bool) error {
	// RE-SEND until the agent CONFIRMS it began a turn — do not send twice and hope.
	//
	// A TUI still finishing startup silently swallows whatever you type: ycode paints
	// a splash, then an LSP/memory/OTel/spinner sequence that can run for several
	// seconds and never fully goes quiet, so waitReady's settle heuristic gives up and
	// sends anyway (by design) — straight into a TUI that eats the keystrokes. The old
	// two-shot × 45s send raced that startup and lost often enough that a coached run
	// looked, from every angle, like a model that had nothing to say (calls=0 at a
	// splash screen — a false verdict about the MODEL for a HARNESS delivery bug).
	//
	// Re-sending every few seconds lands the prompt the instant the input box is live.
	// `turn.start` (a fact the agent reports) is the only accept signal we trust; a
	// byte-growth fallback covers tools that cannot say. Once accepted we stop at once,
	// so at most one or two extra copies are ever sent, and only while nothing has
	// begun — never on top of a running turn.
	s.mu.Lock()
	before := s.buf.Len()
	s.mu.Unlock()

	// Unmetered: Start already gated and will record this opening prompt.
	if err := s.say(prompt); err != nil {
		return fmt.Errorf("chat: could not open the conversation with %s: %w", s.Nick, err)
	}
	if sendOnce {
		// Exact-once callers choose transport acceptance over the generic TUI
		// acknowledgement/retry heuristic. Waiting here cannot add evidence without
		// sending again, and sending again would violate their protocol.
		return nil
	}
	sends := 1 // CAP re-sends: a swallowed send gets another chance, but a working
	// agent (or one whose turn.start we simply failed to parse) is never spammed
	// with the prompt over and over. maxOpenSends keeps the initial burst bounded;
	// after it we only WAIT for confirmation, up to the deadline.
	poll := time.NewTicker(400 * time.Millisecond)
	defer poll.Stop()
	// Re-send on an EXPONENTIAL BACKOFF (5s, 10s, 20s, 40s, 80s…), not a fixed tick.
	// A steerable TUI can be slow to reach input-ready (LSP/memory/provider startup),
	// and a fixed 5s tick either gives up too early (a low cap) or queues a dozen
	// duplicate prompts the agent replays when it finally wakes (no cap). Backoff
	// spreads a HANDFUL of sends across a long window: early ones catch a fast start,
	// later ones catch a slow one, and there are never many. The window is generous
	// because a false "never acknowledged" gets recorded as a model failure.
	backoff := 5 * time.Second
	nextResend := time.Now().Add(backoff)
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return fmt.Errorf("chat: %s exited before it could be asked anything", s.Nick)
		case <-poll.C:
		}

		if s.events != nil {
			if s.events.SawTurnStart() {
				return nil // the agent SAID it started. No guessing.
			}
		} else {
			s.mu.Lock()
			grew := s.buf.Len() - before
			s.mu.Unlock()
			if grew > openAckBytes {
				return nil
			}
		}

		// Not begun yet — the startup most likely ate the last send. Resend on backoff.
		if sends < openingSendCap(sendOnce) && time.Now().After(nextResend) {
			if err := s.say(prompt); err != nil {
				return fmt.Errorf("chat: could not re-open the conversation with %s: %w", s.Nick, err)
			}
			sends++
			backoff *= 2
			nextResend = time.Now().Add(backoff)
		}
	}
	return fmt.Errorf("chat: %s never acknowledged the opening prompt after 180s of retries — it "+
		"accepted the keystrokes and produced nothing. Its TUI was most likely still starting up, or "+
		"its provider auth failed silently. This is a HARNESS failure, not a model failure: do not "+
		"record it as one", s.Nick)
}

// openAckBytes is how much output proves the agent actually took the prompt,
// rather than merely echoing it into its input box.
const openAckBytes = 400

// maxOpenSends bounds how many times the opening prompt is (re)sent before we stop
// and only wait for confirmation. Enough to outlast a slow TUI startup swallowing
// the first keystrokes; few enough that a working agent whose turn.start we failed
// to parse is never spammed. With controlled-mode fast startup it rarely exceeds 1.
const maxOpenSends = 6

func openingSendCap(sendOnce bool) int {
	if sendOnce {
		return 1
	}
	return maxOpenSends
}

// Say puts a line in front of the agent WHILE it is working.
//
// This is the one control surface. `meet say`, `weave say`, `foreman tell` and an
// operator at a keyboard all land here, so they cannot drift into meaning
// different things.
//
// It is also a METERED LLM CALL, and used not to be. Start gates the opening
// prompt and records usage once when the session ends — but a session is a
// CONVERSATION, and every steer that lands here buys another turn against the
// same model. A long coached run could therefore spend an unbounded amount past
// an exhausted budget, because only its first turn was ever asked. Say now goes
// through the same gate as Invoke and Start: refused when the budget is gone,
// recorded when it is spent.
func (s *Session) Say(text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("chat: nothing to say")
	}
	// The bus turn-boundary hook.
	//
	// A queued notification is defined as "read at a turn boundary", and this is
	// that boundary: the instant an agent is handed something to do is the one
	// moment it is guaranteed to be listening. Reading the buffer here — rather
	// than asking the agent to remember to run `bus pending` — is the whole point
	// of the sidecar, which exists because an agent heads-down in a turn cannot
	// reliably decide to go check a channel.
	//
	// It lands in Say for the same reason everything else does: this is the ONE
	// control surface, so meet, weave, foreman and an operator at a keyboard all
	// get the behaviour without four copies that can drift.
	//
	// Before the budget gate, deliberately: the notification text is really sent,
	// so it is really billed. Metering the caller's message alone would understate
	// the turn.
	preparedInbox := bus.PreparePrepend(s.CtlSock, text)
	if s.CtlSock == "" && s.inboxAgent != "" {
		preparedInbox = bus.PrepareForAgent(s.inboxAgent, text)
	}
	if err := preparedInbox.Err(); err != nil {
		return fmt.Errorf("chat: prepare turn inbox: %w", err)
	}
	text = preparedInbox.Text

	if d := s.governTurn(text); !d.Allowed() {
		if d.Action == llmbudget.Queue {
			return fmt.Errorf("chat: LLM budget queued %s for %s: %s", d.Delay, s.Agent, d.Reason)
		}
		return fmt.Errorf("chat: LLM budget blocked %s: %s", s.Agent, d.Reason)
	}
	if s.acp == nil && s.CtlSock == "" {
		return fmt.Errorf("chat: %s has no control channel", s.Nick)
	}
	if err := s.reserveTurnBudget(text); err != nil {
		return err
	}
	if err := s.say(text); err != nil {
		return err
	}
	recordPreambleAdmission(context.Background(), preparedInbox)
	if err := preparedInbox.Commit(); err != nil {
		return fmt.Errorf("chat: turn was delivered but its inbox acknowledgement failed: %w", err)
	}
	// Charged on SEND, not on reply: the turn is bought the moment the agent
	// accepts it, and a session's reply text has no boundary we could bill
	// against anyway (that is what WaitIdle exists to guess at).
	// The turn claim is settled once with the session lifetime; text is estimated.
	return nil
}

// say is the unmetered write to the control channel — for traffic that is not a
// billable turn (the opening prompt, already gated by Start; /quit).
func (s *Session) say(text string) error {
	// On an ACP rung the control channel IS the protocol: a steer is the next
	// prompt in the same session, delivered as a request rather than typed at a
	// TUI that may or may not be listening. Say's budget gate above is unchanged
	// — a turn is billed the same whichever transport carries it.
	if s.acp != nil {
		if err := s.acp.prompt(text); err != nil {
			return err
		}
		s.mu.Lock()
		s.lastSteer = time.Now()
		s.mu.Unlock()
		return nil
	}
	if s.CtlSock == "" {
		return fmt.Errorf("chat: %s has no control channel", s.Nick)
	}
	if err := agentctl.Say(s.CtlSock, text); err != nil {
		return err
	}
	// Restart the idle clock. Without this the NEXT WaitIdle can be satisfied by
	// silence that happened BEFORE the agent was ever asked — see WaitIdle.
	s.mu.Lock()
	s.lastSteer = time.Now()
	s.mu.Unlock()
	return nil
}

// governTurn asks the budget gate about ONE mid-session turn.
//
// Unlike governLaunch it cannot re-route: the session is already running under a
// launched process, so Downgrade/RouteAlt to a different model is not available
// mid-conversation. Those are therefore treated as what they are here — the
// model is allowed to finish this turn, and the routing decision belongs to the
// NEXT Start. Only Block and Queue stop a steer.
func (s *Session) governTurn(text string) llmbudget.Decision {
	if s.launch.ModelName == "" {
		return llmbudget.Decision{Action: llmbudget.Allow}
	}
	r := opaqueBudgetRequest(s.launch, text, false)
	r.AllowPremium = s.allowPremium
	a, e := llmbudget.Preview(context.Background(), r)
	if e != nil {
		return llmbudget.Decision{Action: llmbudget.Block, Reason: e.Error()}
	}
	if a.Decision.Action != llmbudget.Allow {
		return llmbudget.Decision{Action: llmbudget.Block, Reason: a.Decision.Reason}
	}
	return a.Decision
}

// Output is everything the agent has said so far. Safe to call while it is still
// talking.
func (s *Session) Output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Turn marks the current end of the transcript and returns everything written
// since the last mark. Say + WaitIdle + Turn is how a caller that needs a REPLY
// (a foreman recording history, a chair writing minutes) gets one out of a stream
// that has no turn boundaries in it.
func (s *Session) Turn() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// PREFER WHAT THE AGENT SAID IT SAID.
	//
	// The terminal scrape is a RECONSTRUCTION: a pty merges stdout and stderr, so
	// the captured text carries banners, spinners and cursor gymnastics, and
	// SanitizeTurn spends its life guessing which bytes were the answer. When the
	// tool reports the answer as DATA, the guessing stops -- the answer is known.
	if s.reported != "" {
		out := s.reported
		s.reported = ""
		s.mark = s.buf.Len()
		return out
	}

	all := s.buf.String()
	if s.mark > len(all) {
		s.mark = len(all)
	}
	out := all[s.mark:]
	s.mark = len(all)
	return out
}

// WaitIdle blocks until the agent has written nothing for `quiet` — counted from
// the later of its last write and the last thing WE said to it — or until it
// exits, or until ctx ends.
//
// # This is a heuristic and it is named like one
//
// A headless one-shot has a real turn boundary: the process exits. An interactive
// session does not — it just stops typing. Silence is the ONLY signal the terminal
// gives us, so silence is what we use, and callers should size `quiet` to the
// slowest thing the agent might legitimately pause for (a tool call, a model
// re-prompt) rather than to how long a human would wait.
//
// It is deliberately NOT a claim that the turn is complete: an agent that pauses
// long enough looks finished, and an agent that streams a spinner never does. A
// caller that needs certainty about what the agent DID must read the artifacts it
// left behind, not the shape of its output — which is the fleet-evidence rule,
// and the honest reason the first-party harness (structured events, not a scraped
// terminal) is worth building.
func (s *Session) WaitIdle(ctx context.Context, quiet time.Duration) error {
	// AN ACP SESSION DOES NOT GUESS. It waits for the agent to report the turn
	// ended, with a reason attached, and `quiet` is meaningless to it — there is
	// nothing to size against a silence that is not being measured.
	if s.acp != nil {
		return s.waitACPTurn(ctx)
	}

	if quiet <= 0 {
		quiet = 20 * time.Second
	}

	// RACE THE REPORT AGAINST THE GUESS. Whichever arrives first is the answer.
	//
	// The first version ran them in sequence: wait for turn.end, and on failure
	// fall back to silence. That is wrong in a way a test caught immediately —
	// waiting for the report consumed the ENTIRE context budget, so the fallback
	// inherited an already-expired deadline and could never run. The degradation
	// path was unreachable, which meant a tool that declared an event channel and
	// did not deliver one would hang until the caller's timeout rather than quietly
	// doing the sensible thing.
	//
	// Racing is also just the truth of the situation: we do not know which will
	// happen. A tool that reports gives us a real boundary; a tool that does not
	// goes quiet. Wait for either.
	events := make(chan string, 1)
	if s.events != nil {
		go func() {
			text, ok, err := s.events.WaitTurnEnd(ctx)
			if err == nil && ok {
				events <- text
				return
			}
			close(events)
		}()
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-s.done:
			return nil // it exited; that IS a boundary

		case text, ok := <-events:
			if ok {
				// THE TOOL TOLD US. No guessing, no quiet period, and the answer
				// arrives as data rather than as bytes scraped off a terminal.
				s.mu.Lock()
				s.reported = text
				s.mu.Unlock()
				return nil
			}
			// The channel closed without a turn.end: we know NOTHING, which is not
			// the same as "the turn ended". Keep waiting on silence — but SAY SO. A
			// capability that quietly does not work is the exact failure this whole
			// line of work exists to stamp out.
			//
			// Known live gap: ycode emits events from its ONE-SHOT path (RunPrompt)
			// and not from its TUI (RunInteractive) — and the TUI is what steer_exec
			// launches, so a steerable ycode session lands here every time. The TUI's
			// events already flow through ycode's internal bus; what is missing is a
			// sink and ONE vocabulary (the bus says `turn.complete`, the one-shot path
			// says `turn.end` — two names for one thing, inside one binary). Until
			// that is fixed this warning fires, and it should: it is telling the truth
			// about a feature that is plumbed and not yet delivered.
			s.eventsOnce.Do(func() {
				fmt.Fprintf(os.Stderr,
					"chat: %s: declared an event channel but reported no turn.end — "+
						"falling back to the SILENCE heuristic (a turn is GUESSED, not reported)\n",
					s.Nick)
			})
			events = nil // stop selecting on a closed channel

		case <-tick.C:
			// SILENCE COUNTS ONLY FROM THE MOMENT WE SPOKE.
			//
			// The reference point is the LATER of "it last wrote" and "we last
			// steered it", and the difference is the whole correctness of this
			// function. Measured from the last write alone, an agent that has been
			// thinking — or simply idle since the session opened — is already past
			// `quiet` at the instant a steer is handed to it, so the very first tick
			// after Say declares the turn over before the agent has typed a
			// character. The caller gets an empty Turn() and reports that the agent
			// produced no output, which is true of the bytes and false of the agent.
			//
			// Restarting the clock at the steer makes `quiet` mean what every caller
			// already assumed: the agent gets that long to START, and each write buys
			// it that long again to continue. An agent that really does answer
			// nothing still terminates the wait, one `quiet` after being asked.
			s.mu.Lock()
			last, steer := s.lastWrite, s.lastSteer
			s.mu.Unlock()
			if steer.After(last) {
				last = steer
			}
			if !last.IsZero() && time.Since(last) >= quiet {
				return nil
			}
		}
	}
}

// Live reports whether the agent is still running.
func (s *Session) Live() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Wait blocks until the agent exits, and reports how.
func (s *Session) Wait() (int, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.exit, s.err
	}
	if s.killed != "" {
		return s.exit, fmt.Errorf("agent terminated: %s", s.killed)
	}
	return s.exit, nil
}

// EventsPath is the tool's structured event file for this session, or "" when
// the tool has no event channel (only ycode declares `events_arg:` today). A
// coach tails this to see tool.call as STRUCTURED DATA rather than scraping the
// terminal — the difference between the two the whole event channel exists for.
func (s *Session) EventsPath() string {
	if s.events != nil {
		return s.events.path
	}
	return ""
}

// Interrupt breaks the agent out of the turn it is running (ESC).
//
// A Say is a word in the agent's ear, and every agent TUI in this fleet QUEUES
// it and reads it only when the current turn ends — useless for the one case a
// coach exists for: an agent stuck in a tool loop whose turn is never going to
// end. Escape is the only thing that reaches it there. Mirrors foreman's
// TrySendKey("esc"). No-op if the session is not live.
func (s *Session) Interrupt() error {
	if !s.Live() {
		return nil
	}
	// ACP has a first-class cancellation: the turn comes back with
	// StopReasonCancelled as a SUCCESSFUL response, so an interrupt is
	// acknowledged rather than merely sent into a terminal and hoped for.
	if s.acp != nil {
		return s.acp.client.Cancel(context.Background(), s.acp.session)
	}
	return agentpty.SendFrame(s.CtlSock, agentpty.VerbatimFrame([]byte{0x1b}))
}

// Quit asks the agent to leave, rather than killing it.
//
// Asking is not the same as making it go: a tool that ignores the request is
// ended by the caller's timeout or by Close. But an agent given the chance to
// finish writing usually leaves a cleaner tree than one shot mid-edit.
// Quit is a control command, not a turn: it buys no model work, so it is never
// gated. An exhausted budget must still be able to shut a session down.
func (s *Session) Quit() error {
	// "/quit" is a TUI slash command, and an ACP agent has no TUI: sending it
	// would buy a turn in which the agent dutifully explains that it cannot quit.
	// The protocol's own shutdown is closing the connection.
	if s.acp != nil {
		s.closeACP()
		return nil
	}
	return s.say("/quit")
}

// Close ends the session now.
func (s *Session) Close() {
	if s.acp != nil {
		s.closeACP()
		return
	}
	s.closePTY(5 * time.Second)
	_ = os.Remove(s.CtlSock)
}

// abortStart tears down a PTY that launched but failed before Start could hand
// a usable session to its caller. There is no graceful conversation to preserve
// on this path: cancel immediately so a readiness, delivery, or inbox-ack error
// cannot strand a process and room claim that no supervisor owns.
func (s *Session) abortStart() {
	if s.acp != nil {
		s.closeACP()
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
	}
	_ = os.Remove(s.CtlSock)
}

func (s *Session) closePTY(grace time.Duration) {
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.Quit()
	select {
	case <-s.done:
		return
	case <-time.After(grace):
	}
	if s.cancel != nil {
		s.cancel()
	}
	// agentpty escalates a cancelled process tree after two seconds. Bound this
	// wait as well: Close must never deadlock its supervisor on a broken tool.
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
	}
}

// waitReady blocks until the control socket exists, so the first message is not
// sent into a socket agentpty has not bound yet — which would silently vanish.
// waitReady blocks until the agent can actually HEAR us.
//
// Two conditions, and the second one is the one that was missing:
//
//  1. The control socket is bound — otherwise the message goes to an address that
//     does not exist yet and simply vanishes.
//  2. The TUI has DRAWN and gone QUIET. A tool still starting up (opencode loads
//     MCP servers; codex draws a splash) swallows keystrokes without a trace. It
//     is not an error, it is not a rejection, it is nothing at all — the session
//     just sits at an empty prompt forever, looking for all the world like a model
//     with nothing to say.
//
// This used to be `time.Sleep(1500ms)`, which is not a readiness check; it is a
// guess. It happened to be long enough for the tools that take their prompt on
// argv (where it does not matter) and too short for the ones that do not (where it
// is the whole ballgame). Waiting for the output to settle asks the tool itself
// whether it is ready, instead of betting on a number.
func (s *Session) waitReady(ctx context.Context) error {
	// 1. The socket.
	deadline := time.Now().Add(20 * time.Second)
	bound := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(s.CtlSock); err == nil {
			bound = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return fmt.Errorf("chat: %s exited before its session was ready", s.Nick)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if !bound {
		return fmt.Errorf("chat: %s never opened a control channel", s.Nick)
	}

	// 2. The TUI: it has painted something, and then stopped painting.
	//
	// Bounded, because a tool that renders a spinner never goes quiet at all. When
	// the budget runs out we send anyway — a prompt that might be swallowed beats a
	// session that never starts, and openConversation confirms delivery regardless.
	settle := 1200 * time.Millisecond
	budget := time.Now().Add(25 * time.Second)
	for time.Now().Before(budget) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return fmt.Errorf("chat: %s exited before its session was ready", s.Nick)
		case <-time.After(200 * time.Millisecond):
		}
		s.mu.Lock()
		drawn, quiet := s.buf.Len() > 0, time.Since(s.lastWrite) >= settle
		s.mu.Unlock()
		if drawn && quiet {
			return nil
		}
	}
	return nil
}

type sessionWriter struct{ s *Session }

func (w *sessionWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	w.s.buf.Write(p)
	w.s.lastWrite = time.Now()
	w.s.mu.Unlock()
	return len(p), nil
}

// CanSteer reports whether an agent has an interactive launch, and if not, why.
//
// Callers use this to degrade LOUDLY. A conductor that silently falls back to
// replaying the conversation into a fresh one-shot — which is what `foreman tell`
// used to do — looks exactly like one that steered, and the operator has no way to
// tell that their mid-turn correction actually arrived after the turn was over.
func CanSteer(agent string) (bool, string) {
	if !agentpty.Supported() {
		return false, "this platform has no pty support"
	}
	name, err := ResolveAgent(agent, "")
	if err != nil {
		return false, err.Error()
	}
	if _, err := resolveLaunch(name, Options{Steer: true, ReadOnly: true}); err != nil {
		return false, err.Error()
	}
	return true, ""
}
