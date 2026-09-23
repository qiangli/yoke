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
	"sync/atomic"
	"time"

	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/room"
)

// principalName is the human/agent this session is attributed to on its room card.
// Optional (best-effort from the environment) — the card omits it when unknown.
func principalName() string {
	for _, k := range []string{"BASHY_PRINCIPAL", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// InteractOptions configures a foreground, human-driven session.
type InteractOptions struct {
	// Prompt optionally opens the conversation. A tool whose steerable launch
	// takes a prompt on the command line gets it there; one that opens an empty
	// session is left at its prompt for the human to type into.
	Prompt   string
	Cwd      string
	Timeout  time.Duration
	ReadOnly bool
	// Unattended (--yolo) keeps the agent's approval gate OFF for a session
	// supervised remotely via steer — no one sits at the terminal to approve.
	Unattended bool
	// AllowPremium explicitly bypasses LLM budget/subscription gates for urgent
	// human-authorized work.
	AllowPremium bool
	// Status receives bashy's own one-line notices (which agent, the session id);
	// defaults to os.Stderr so they never contaminate the tool's stdout.
	Status io.Writer
	// Attach opts in to joining the agent's LIVE session when it already has one,
	// instead of being refused.
	//
	// Opt-IN, not the default. An agent is one identity, so `--agent X` when X is
	// live cannot mean "start a second X" — but silently making it mean "watch the
	// session someone else is driving" would be its own surprise: the operator
	// asked to start an agent and would get a spectator seat, with their typing
	// going somewhere they did not expect. So the default refuses and names this
	// flag, and the flag does exactly what it says.
	Attach bool
}

// nativeHarnesses are first-party tools that already speak bashy's event channel
// and governance directly — wrapping them in a chat session adds nothing. Kept as
// a set so it is one obvious place to extend if another first-party harness lands.
var nativeHarnesses = map[string]bool{"ycode": true}

// Interact launches an agent's NATIVE interactive session in the foreground, under
// full bashy governance, and registers it so the fleet can reach it.
//
// It is the foreground twin of Start: the SAME resolveLaunch + agentChildEnv, so
// governance (vault-secret scrub, single declared API key, shell forced to bashy,
// principal identity) never drifts between a programmatic session and a human one.
// The only difference is that agentpty.Run is called in the FOREGROUND with
// Capture:false, so the human's terminal IS the session and the tool's own TUI is
// what they see — identical to running the tool directly, but with the selected
// model, governed, and addressable (steer / interrupt / attach / observe).
func Interact(ctx context.Context, agent string, opt InteractOptions) (int, error) {
	status := opt.Status
	if status == nil {
		status = os.Stderr
	}

	if !agentpty.Supported() {
		return 1, fmt.Errorf("chat: an interactive session needs a pty, which this platform has no support for")
	}
	name, err := ResolveAgent(agent, "")
	if err != nil {
		return 1, err
	}
	// Deliberately the SAME resolver and launch Start/Invoke use — a session
	// differs only in which launch template it renders, and governance must not
	// be able to drift. Attended: a human is driving, so the tool's own approval
	// gate stays ON (the auto-approve kill-switches are stripped) — safer than an
	// unattended fleet launch, and it passes the uncontained-host guard exactly as
	// running the tool by hand would. ReadOnly, when set, is stricter and wins.
	l, err := resolveLaunch(name, Options{
		Cwd:         opt.Cwd,
		ReadOnly:    opt.ReadOnly,
		Attended:    !opt.ReadOnly,
		AllowUnsafe: opt.Unattended,
		Steer:       true,
	})
	if err != nil {
		return 1, err
	}
	next, d, err := governLaunch(ctx, name, l, opt.Prompt, Options{
		Cwd:          opt.Cwd,
		ReadOnly:     opt.ReadOnly,
		Attended:     !opt.ReadOnly,
		AllowUnsafe:  opt.Unattended,
		AllowPremium: opt.AllowPremium,
		Steer:        true,
	})
	if err != nil {
		return 1, err
	}
	if d.Action == llmbudget.Queue {
		return 75, fmt.Errorf("chat: LLM budget queued %s for %s: %s", d.Delay, l.Binding(), d.Reason)
	}
	if d.Action == llmbudget.Block {
		return 75, fmt.Errorf("chat: LLM budget blocked %s: %s", l.Binding(), d.Reason)
	}
	l = next

	// ycode (and any first-party harness) applies its OWN governance, so bashy does
	// not scrub its env or force a single key — that is the only difference (native
	// env, below). It still routes through the SAME pty + control socket + host-room
	// membership as every other agent, because that is the whole point of
	// `bashy chat`: an instance launched this way is DISCOVERABLE and STEERABLE.
	// (An earlier version launched ycode with inherited stdio and no control channel
	// — a running session was then invisible on `chat sessions` and impossible to
	// steer. This closes that gap; the transparent pty passthrough keeps the UX
	// identical to `ycode --model x`.)
	native := nativeHarnesses[l.ToolName]

	argv := append([]string{}, l.Args...)

	cwd := opt.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	id := agentID(l)

	sock, err := sessionSock(id)
	if err != nil {
		return 1, err
	}

	// CLAIM THE IDENTITY FIRST, before anything with a side effect.
	//
	// The claim has to precede sessionLog in particular: that call TRUNCATES the
	// capture, so checking afterwards would have already destroyed the live
	// session's transcript on its way to telling the operator it could not start.
	// The card is completed and re-joined below once the log path exists —
	// re-joining an id this process already holds is an update, by design.
	card := room.Card{
		ID:        id,
		Principal: principalName(),
		Tool:      l.ToolName,
		Model:     l.ModelName,
		Binding:   l.Binding(),
		Nick:      l.Nick,
		Band:      bindingBand(name),
		Mode:      "interactive",
		CtlSock:   sock,
		PID:       os.Getpid(),
		Cwd:       cwd,
		Native:    native,
		Caps:      []string{room.CapInboxDelivery},
	}
	if err := room.Join(card); err != nil {
		var live *room.ErrLive
		if errors.As(err, &live) {
			prior, _, _ := room.Find(live.ID)
			if opt.Attach {
				if err := AttachTo(ctx, os.Stdin, os.Stdout, status, prior); err != nil {
					return 1, err
				}
				return 0, nil
			}
			return 1, errAgentLive(live.ID, live.PID, prior.Cwd)
		}
		return 1, err
	}
	defer room.Leave(card.ID)
	releaseInbox, err := bus.BindInstance(name, card.ID)
	if err != nil {
		return 1, fmt.Errorf("chat: bind %s inbox to live session: %w", name, err)
	}
	defer func() { _ = releaseInbox() }()

	// A capture log ALONGSIDE the native TUI is what makes a live human session
	// observable/attachable. Best-effort: if it cannot be opened the session still
	// runs (native UX intact), it just is not followable.
	logPath, logSink, closeLog := sessionLog(id)
	if closeLog != nil {
		defer closeLog()
	}
	usage := &usageCountingWriter{}
	if logSink != nil {
		logSink = io.MultiWriter(logSink, usage)
	} else {
		logSink = usage
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Same resolution as agentCommand: a supervised daemon's PATH is not the
	// operator's, and a live session must not be reachable only from a terminal.
	tool := l.Tool
	if p := ResolveToolBinary(tool); p != "" {
		tool = p
	}
	cmd := exec.CommandContext(runCtx, tool, argv...)
	cmd.Dir = cwd
	if native {
		// Native env: a first-party harness uses the operator's real env + its own
		// key/config, so it is truly "identical to `ycode --model x`" for auth. bashy
		// adds only the control channel + membership, never a governance override.
		// The per-user binary homes are still appended — that is the environment
		// the operator's own shell has, not a governance override.
		cmd.Env = toolPathEnv(os.Environ())
	} else {
		// agentChildEnv is the one choke point that scrubs the operator's vault
		// secrets, grants back only this model's one API key, forces the child's
		// shell to bashy, and stamps its principal identity. Built anywhere else, all
		// four silently drop.
		cmd.Env = agentChildEnv(withLaunch(ctx, l))
	}
	if p, ok := agentctl.ProfileFor(l.ToolName); ok && p.Preseed != "" {
		_ = agentctl.ApplyTrustPreseed(cmd.Dir, p.Preseed)
	}

	card.LogPath = logPath
	_ = room.Join(card) // update: we already hold this id

	posture := "governed + steerable"
	if native {
		posture = "native harness (self-governing) + steerable"
	}
	fmt.Fprintf(status, "chat: %s (%s) — %s (id %s). Ctrl-C ends it; "+
		"from another terminal: `bashy chat steer %s \"...\"` / `attach %s`.\n",
		card.Nick, card.Binding, posture, card.ID, card.ID, card.ID)

	// An interactive instruction must go through the live terminal, after the TUI
	// has actually drawn and settled. Passing it on argv works for only some tools;
	// sending it as soon as the socket appears races startup and can be swallowed.
	var (
		ready      *interactiveReady
		delivered  chan error
		runDone    chan struct{}
		promptText = opt.Prompt
		inboxReady atomic.Bool
	)
	if strings.TrimSpace(promptText) == "" {
		inboxReady.Store(true)
	}
	if strings.TrimSpace(promptText) != "" {
		ready = &interactiveReady{}
		if logSink != nil {
			logSink = io.MultiWriter(logSink, ready)
		} else {
			logSink = ready
		}
		delivered = make(chan error, 1)
		runDone = make(chan struct{})
		go func() {
			err := deliverInteractivePrompt(runCtx, card.Nick, sock, promptText, ready, runDone,
				func(text string) error { return agentctl.Say(sock, text) })
			if err != nil {
				cancel() // do not leave a session running after silently losing its instruction
			} else {
				inboxReady.Store(true)
				fmt.Fprintf(status, "chat: instruction delivered to %s\n", card.Nick)
			}
			delivered <- err
		}()
	}

	// A foreground session is still a managed agent session. Printing a watcher
	// in another terminal cannot wake the model; inject the prepared inbox block
	// through this session's control socket and acknowledge it only after the
	// socket accepted the steer. Agent TUIs queue that steer for their next turn.
	// When there is an opening instruction, let it land first so two control
	// writers cannot race during TUI startup.
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	// BASHY_CHAT_INBOX=off keeps the host's mail out of the session. The agent
	// bench needs it: every run must see the same input, and board traffic is
	// neither fixed nor part of any task.
	if os.Getenv("BASHY_CHAT_INBOX") != "off" {
		go runInboxRelay(ctx, sessionDone, inboxReady.Load,
			func() bus.PreparedPreamble { return bus.PrepareForAgent(name, "") },
			func(p bus.PreparedPreamble) error {
				if err := agentctl.Say(sock, p.Text); err != nil {
					return err
				}
				recordPreambleAdmission(context.Background(), p)
				return p.Commit()
			}, bus.NewInboxPollGate(name))
	}

	// Foreground + parent-is-a-TTY + Capture:false → agentpty gives native raw-mode
	// passthrough (the tool's own TUI), teeing to logSink for observers.
	exit, killed, runErr := agentpty.Run(cmd, logSink, agentpty.Options{
		CtlSock:    sock,
		Capture:    false,
		MaxRuntime: opt.Timeout,
	})
	if runDone != nil {
		close(runDone)
		if err := <-delivered; err != nil {
			return exit, err
		}
	}
	if killed != "" {
		fmt.Fprintf(status, "chat: session ended (%s)\n", killed)
	}
	recordLaunchUsageTokens(ctx, l, estimateTokens(opt.Prompt), usage.Tokens())
	return exit, runErr
}

type usageCountingWriter struct {
	bytes atomic.Int64
}

func (w *usageCountingWriter) Write(p []byte) (int, error) {
	w.bytes.Add(int64(len(p)))
	return len(p), nil
}

func (w *usageCountingWriter) Tokens() int64 {
	n := w.bytes.Load() / 4
	if w.bytes.Load() > 0 && n == 0 {
		n = 1
	}
	return n
}

type interactiveReady struct {
	mu        sync.Mutex
	drawn     bool
	lastWrite time.Time
}

func (r *interactiveReady) Write(p []byte) (int, error) {
	r.mu.Lock()
	if len(p) > 0 {
		r.drawn = true
		r.lastWrite = time.Now()
	}
	r.mu.Unlock()
	return len(p), nil
}

func (r *interactiveReady) settled(forDuration time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drawn && time.Since(r.lastWrite) >= forDuration
}

type interactiveDeliveryTiming struct {
	poll, socketTimeout, settle, readyTimeout time.Duration
}

var defaultInteractiveDeliveryTiming = interactiveDeliveryTiming{
	poll: 100 * time.Millisecond, socketTimeout: 20 * time.Second,
	settle: 1200 * time.Millisecond, readyTimeout: 25 * time.Second,
}

func deliverInteractivePrompt(ctx context.Context, nick, sock, prompt string, ready *interactiveReady,
	runDone <-chan struct{}, send func(string) error) error {
	return deliverInteractivePromptWithTiming(ctx, nick, sock, prompt, ready, runDone, send,
		defaultInteractiveDeliveryTiming)
}

func deliverInteractivePromptWithTiming(ctx context.Context, nick, sock, prompt string, ready *interactiveReady,
	runDone <-chan struct{}, send func(string) error, timing interactiveDeliveryTiming) error {
	wait := func(deadline time.Time, condition func() bool, failure string) error {
		for !condition() {
			if time.Now().After(deadline) {
				return fmt.Errorf("chat: %s %s", nick, failure)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-runDone:
				return fmt.Errorf("chat: %s exited before its instruction was delivered", nick)
			case <-time.After(timing.poll):
			}
		}
		return nil
	}
	if err := wait(time.Now().Add(timing.socketTimeout), func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, "never opened a control channel for its instruction"); err != nil {
		return err
	}
	if err := wait(time.Now().Add(timing.readyTimeout), func() bool {
		return ready.settled(timing.settle)
	}, "TUI never became ready for its instruction"); err != nil {
		return err
	}
	if err := send(prompt); err != nil {
		return fmt.Errorf("chat: could not deliver the instruction to %s: %w", nick, err)
	}
	return nil
}

// bindingBand is the capability band of a resolved agent name, or 0 when the name
// is a bare tool that pegs to no model. Best-effort decoration for `chat sessions`.
func bindingBand(name string) int {
	if _, _, m, err := newCatalog().Binding(name); err == nil {
		return m.Band
	}
	return 0
}
