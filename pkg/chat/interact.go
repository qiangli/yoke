package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/room"
)

// principalName is the human/agent this session is attributed to on its room card.
// Optional (best-effort from the environment) — the card omits it when unknown.
func principalName() string {
	for _, k := range []string{"BASHY_PRINCIPAL", "USER", "LOGNAME", "USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return osAccountName()
}

// osAccountName names the OS account when the environment does not: a
// container, cron job or service often sets no USER, and a queued steer is a
// board post, which needs a sender. A uid with no passwd entry is named by
// number rather than left empty.
func osAccountName() string {
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return strings.TrimSpace(u.Username)
	}
	if uid := os.Getuid(); uid >= 0 {
		return fmt.Sprintf("uid-%d", uid)
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
	// DenyUnsafe (--no-yolo) keeps the approval gate ON wherever the session
	// runs; the command-line flag outranks containment and the environment.
	DenyUnsafe bool
	// NoTrust leaves the vendor trust dialog to the terminal operator.
	NoTrust bool
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
		DenyUnsafe:  opt.DenyUnsafe,
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
		DenyUnsafe:   opt.DenyUnsafe,
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
		Caps:      []string{room.CapInboxDelivery, newSteerInboxCap()},
		Joined:    time.Now().UTC().Format(time.RFC3339),
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
			// A human starting an agent that is already live gets the next free
			// instance of it (name+2, name+3, …) with its own context, inbox and
			// socket — never a second session inside the first one's identity.
			if err := joinNextInstance(&card); err != nil {
				return 1, err
			}
			fmt.Fprintf(status, "chat: %s is already live (pid %d) — starting instance %s\n", live.ID, live.PID, card.ID)
			id, sock, name = card.ID, card.CtlSock, card.ID
			l.Nick = card.Nick
		} else {
			return 1, err
		}
	}
	defer room.Leave(card.ID)
	instCtx, sessionClaim, releaseSessionInst := openInteractiveInstance(&card, l, cwd)
	if releaseSessionInst != nil {
		defer releaseSessionInst()
	}
	inboxAddr := name
	if instCtx.Instance.UUID != "" {
		inboxAddr = instCtx.Instance.MailAddress()
		card.Principal = instCtx.Instance.URN()
		card.SessionClaim = sessionClaim
		_ = room.Join(card)
	}
	releaseInbox, err := bus.BindInstance(inboxAddr, card.ID)
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
	tool, err := interactiveBinary(runCtx, l)
	if err != nil {
		return 1, err
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
	if instCtx.Instance.UUID != "" {
		cmd.Env = agentlaunch.ContextEnv(cmd.Env, instCtx)
		cmd.Env = agentlaunch.SessionEnv(cmd.Env, sessionClaim)
		cmd.Env = withChildEnvKV(cmd.Env, "BASHY_AGENT", card.ID)
	}
	dialogs := interactiveTrust(cmd, l.ToolName, opt, status)

	card.LogPath = logPath
	_ = room.Join(card) // update: we already hold this id

	posture := "governed + steerable"
	if native {
		posture = "native harness (self-governing) + steerable"
	}
	who := card.Nick
	if card.Binding != "" {
		who += " (" + card.Binding + ")"
	}
	fmt.Fprintf(status, "chat: %s — %s (id %s, %s). Ctrl-C ends it; "+
		"from another terminal: `bashy chat steer %s \"...\"` / `attach %s`.\n",
		who, posture, card.ID, hostVersionLabel(), card.ID, card.ID)
	fmt.Fprintln(status, startedLine(name, os.Getenv("BASHY_CHAT_INBOX") != "off"))

	// An interactive instruction must go through the live terminal, after the TUI
	// has actually drawn and settled. Passing it on argv works for only some tools;
	// sending it as soon as the socket appears races startup and can be swallowed.
	//
	// The inbox relay waits for the same screen: drawn, quiet and gate-free. A
	// mail block typed into a TUI that is still starting, or into its trust
	// dialog, is lost at best.
	var (
		ready           = &interactiveReady{dialogs: dialogs}
		delivered       chan error
		runDone         chan struct{}
		promptText      = opt.Prompt
		promptDelivered atomic.Bool
	)
	if logSink != nil {
		logSink = io.MultiWriter(logSink, ready)
	} else {
		logSink = ready
	}
	relay := startSteerRelay(card, 0, promptDelivered.Load, managedInboxPoll, defaultInteractiveDeliveryTiming.readyTimeout, status, dialogs)
	logSink = io.MultiWriter(logSink, relay)
	inboxReady := func() bool {
		return promptDelivered.Load() && ready.settled(defaultInteractiveDeliveryTiming.settle)
	}
	if strings.TrimSpace(promptText) == "" {
		promptDelivered.Store(true)
	}
	if strings.TrimSpace(promptText) != "" {
		delivered = make(chan error, 1)
		runDone = make(chan struct{})
		go func() {
			err := deliverInteractivePrompt(runCtx, card.Nick, sock, promptText, ready, runDone, status,
				func(text string) error { return dialogs.Send(agentpty.TextFrame(text)) })
			if err != nil {
				cancel() // do not leave a session running after silently losing its instruction
			} else {
				fmt.Fprintf(status, "chat: instruction delivered to %s\n", card.Nick)
				promptDelivered.Store(true)
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
		mail := newPTYInbox(inboxAddr)
		go runInboxRelay(ctx, sessionDone, inboxReady,
			func() bus.PreparedPreamble { return bus.PrepareForAgent(inboxAddr, "") },
			func(p bus.PreparedPreamble) error {
				complete, err := mail.deliver(p.Text, func(text string) error { return dialogs.Send(agentpty.TextFrame(text)) })
				if err != nil || !complete {
					return err
				}
				recordPreambleAdmission(context.Background(), p)
				return p.Commit()
			}, bus.NewInboxPollGate(inboxAddr))
	}

	// Foreground + parent-is-a-TTY + Capture:false → agentpty gives native raw-mode
	// passthrough (the tool's own TUI), teeing to logSink for observers.
	exit, killed, runErr := agentpty.Run(cmd, logSink, agentpty.Options{
		CtlSock:    sock,
		Capture:    false,
		MaxRuntime: opt.Timeout,
		OnResize:   publishGeometry(card),
		Dialogs:    dialogs,
	})
	relay.Close()
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
	if runErr != nil && cmd.Process == nil {
		runErr = spawnError(l, tool, runErr)
	}
	return exit, runErr
}

func interactiveTrust(cmd *exec.Cmd, tool string, opt InteractOptions, status io.Writer) *agentpty.DialogGuard {
	if opt.NoTrust || !opt.Unattended || opt.ReadOnly || nativeHarnesses[tool] {
		return agentpty.NewDialogGuard(nil)
	}
	if p, ok := agentctl.ProfileFor(tool); ok {
		entry, err := agentctl.ApplyLaunchTrustPreseed(cmd.Dir, p.Preseed, cmd.Env)
		if err != nil {
			fmt.Fprintf(status, "chat: %s trust preseed failed for %q: %v\n", tool, cmd.Dir, err)
		} else if entry != "" {
			fmt.Fprintf(status, "chat: %s trust preseed: %s\n", tool, entry)
		}
	}
	return agentpty.NewDialogGuard(func(label string) {
		fmt.Fprintf(status, "chat: answered trust dialog for %q (%s selected; preseed did not clear it)\n", cmd.Dir, label)
	})
}

// interactiveBinary is the executable a live session spawns. A managed tool's
// Launch.Tool is only the COMPUTED cache path of its pin (resolveManaged never
// downloads), so a session must install the pin first, exactly as execRunner
// and agentCommand do; spawning the path as-is died with ENOENT on any host
// that had not yet run a one-shot of the tool. No PATH fallback: a failed
// install is the launch's failure, named with its override.
func interactiveBinary(ctx context.Context, l Launch) (string, error) {
	tool := l.Tool
	if l.ManagedTool != nil {
		p, err := agentlaunch.EnsureManaged(ctx, toAgentLaunch(l))
		if err != nil {
			return "", err
		}
		tool = p
	}
	if p := ResolveToolBinary(tool); p != "" {
		tool = p
	}
	return tool, nil
}

// spawnError names what a session that never started tried to run: the
// resolved path and the OS error, plus the override a managed tool offers.
func spawnError(l Launch, path string, err error) error {
	msg := fmt.Sprintf("chat: could not start %s at %s", l.ToolName, path)
	if l.ManagedTool != nil {
		msg += fmt.Sprintf(" (managed pin %s; set %s to launch another binary explicitly)",
			l.ManagedTool.Version, fleet.ToolBinaryOverrideEnv(l.ToolName))
	}
	return fmt.Errorf("%s: %w", msg, err)
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

// interactiveReady decides when the agent's TUI can take typed input: it has
// drawn, the output has gone quiet, and what it drew is not a gate. A still
// dialog is not a ready prompt — codex's folder-trust dialog sits motionless,
// and an instruction typed into it quits codex.
type interactiveReady struct {
	mu        sync.Mutex
	drawn     bool
	lastWrite time.Time
	tail      string
	dialogs   *agentpty.DialogGuard
}

// readyTailBytes bounds the output the gate check reads: a dialog is one frame.
const readyTailBytes = 8 << 10

func (r *interactiveReady) Write(p []byte) (int, error) {
	r.mu.Lock()
	if len(p) > 0 {
		r.drawn = true
		r.lastWrite = time.Now()
		if r.dialogs == nil {
			r.tail += string(p)
			if len(r.tail) > readyTailBytes {
				r.tail = r.tail[len(r.tail)-readyTailBytes:]
			}
		}
	}
	r.mu.Unlock()
	return len(p), nil
}

func (r *interactiveReady) settled(forDuration time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.drawn || time.Since(r.lastWrite) < forDuration {
		return false
	}
	return r.clear()
}

func (r *interactiveReady) clear() bool {
	if r.dialogs != nil {
		return r.dialogs.Clear()
	}
	return agentpty.ClassifyGate(r.tail).Kind == agentpty.GateNone
}

func (r *interactiveReady) dialogActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.clear()
}

// gateAnswered forgets the output so far: the trust tap answered the dialog in
// it, and only what the TUI draws next says whether it is ready.
func (r *interactiveReady) gateAnswered(agentpty.GateVerdict, string) {
	r.mu.Lock()
	r.tail = ""
	r.lastWrite = time.Now()
	r.mu.Unlock()
}

type interactiveDeliveryTiming struct {
	poll, socketTimeout, settle, readyTimeout time.Duration
}

var defaultInteractiveDeliveryTiming = interactiveDeliveryTiming{
	poll: 100 * time.Millisecond, socketTimeout: 20 * time.Second,
	settle: 1200 * time.Millisecond, readyTimeout: 25 * time.Second,
}

func deliverInteractivePrompt(ctx context.Context, nick, sock, prompt string, ready *interactiveReady,
	runDone <-chan struct{}, status io.Writer, send func(string) error) error {
	return deliverInteractivePromptWithTiming(ctx, nick, sock, prompt, ready, runDone, status, send,
		defaultInteractiveDeliveryTiming)
}

func deliverInteractivePromptWithTiming(ctx context.Context, nick, sock, prompt string, ready *interactiveReady,
	runDone <-chan struct{}, status io.Writer, send func(string) error, timing interactiveDeliveryTiming) error {
	wait := func(deadline time.Time, condition func() bool, timeoutErr error) error {
		for !condition() {
			if time.Now().After(deadline) {
				return timeoutErr
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
	}, fmt.Errorf("chat: %s never opened a control channel for its instruction", nick)); err != nil {
		return err
	}
	readyTimeout := errors.New("TUI did not settle")
	err := wait(time.Now().Add(timing.readyTimeout), func() bool {
		return ready.settled(timing.settle)
	}, readyTimeout)
	if err != nil && err != readyTimeout {
		return err
	}
	timedOut := err == readyTimeout
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-runDone:
			return fmt.Errorf("chat: %s exited before its instruction was delivered", nick)
		default:
		}
		if !ready.dialogActive() {
			if timedOut {
				fmt.Fprintf(status, "chat: warning: %s TUI did not settle within %s; delivering the instruction anyway\n", nick, timing.readyTimeout)
				timedOut = false
			}
			err := send(prompt)
			if !errors.Is(err, agentpty.ErrDialogActive) {
				if err != nil {
					return fmt.Errorf("chat: could not deliver the instruction to %s: %w", nick, err)
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
		case <-runDone:
		case <-time.After(timing.poll):
		}
	}
}

// bindingBand is the capability band of a resolved agent name, or 0 when the name
// is a bare tool that pegs to no model. Best-effort decoration for `chat sessions`.
// startedLine tells the human which inbox this session watches, so mail for
// the agent has an obvious address.
func startedLine(agent string, inbox bool) string {
	if !inbox {
		return fmt.Sprintf("chat: %q started — inbox delivery off (BASHY_CHAT_INBOX=off)", agent)
	}
	return fmt.Sprintf("chat: %q started — watching inbox %s (`bashy mb send %s \"...\"`)", agent, agent, agent)
}

func bindingBand(name string) int {
	if _, _, m, err := newCatalog().Binding(name); err == nil {
		return m.Band
	}
	return 0
}

// publishGeometry keeps the card's Cols/Rows equal to the session PTY's size,
// so a mirror replaying LogPath draws the TUI at the width it was drawn for.
// Best-effort: a failed update costs a mirror its geometry, never the session.
func publishGeometry(card room.Card) func(rows, cols uint16) {
	var mu sync.Mutex
	return func(rows, cols uint16) {
		mu.Lock()
		defer mu.Unlock()
		if card.Rows == int(rows) && card.Cols == int(cols) {
			return
		}
		card.Rows, card.Cols = int(rows), int(cols)
		_ = room.Join(card) // update: this process holds the id
	}
}

// HostVersion reports the running bashy release ("1.0.0-rc.39"); bashy wires
// it at startup. Printed in the session banner so a human sees which build
// they are on.
var HostVersion = func() string { return "" }

func hostVersionLabel() string {
	if v := strings.TrimSpace(HostVersion()); v != "" {
		return "bashy v" + strings.TrimPrefix(v, "v")
	}
	return "bashy dev"
}

// joinNextInstance claims the first free name+N room key (N from 2) for card,
// moving its control socket with it. A live holder of a candidate is skipped.
func joinNextInstance(card *room.Card) error {
	base := card.ID
	for n := 2; n < 100; n++ {
		id := room.AgentClaimID(base + "+" + strconv.Itoa(n))
		sock, err := sessionSock(id)
		if err != nil {
			return err
		}
		next := *card
		next.ID, next.CtlSock = id, sock
		next.Nick = id
		err = room.Join(next)
		if err == nil {
			*card = next
			return nil
		}
		var live *room.ErrLive
		if !errors.As(err, &live) {
			return err
		}
	}
	return fmt.Errorf("chat: %s has no free instance up to %s+99", base, base)
}

// openInteractiveInstance opens a fresh UUID-keyed fleet.Instance for an
// interactive session so NAME and NAME+N each own an isolated mailbox, cursor,
// and sender snapshot, and retires it on exit.
func openInteractiveInstance(card *room.Card, l Launch, cwd string) (agentlaunch.InstanceContext, string, func()) {
	store := bus.InstanceStoreFn()
	if store == nil {
		return agentlaunch.InstanceContext{}, "", nil
	}
	fam, err := agentlaunch.FamilyForLaunch(toAgentLaunch(l), nil)
	if err != nil {
		tool := strings.TrimSpace(l.ToolName)
		if tool == "" {
			tool = strings.TrimSpace(card.Tool)
		}
		if tool == "" {
			return agentlaunch.InstanceContext{}, "", nil
		}
		fam = fleet.Family{
			Name:     tool,
			Display:  tool,
			Policy:   fleet.PolicySingle,
			Bindings: []string{tool},
		}
	}
	mailFrom := bus.BoardHeadSeq() + 1
	inst, err := store.Open(fam, fleet.OpenOptions{
		Label:    card.ID,
		MailFrom: mailFrom,
	})
	if err != nil {
		return agentlaunch.InstanceContext{}, "", nil
	}
	sessionClaim := agentlaunch.CurrentSessionClaim()
	if sessionClaim == "" {
		sessionClaim = bus.HashSessionClaim(card.ID + ":" + inst.UUID)
	}
	_, _ = principal.ClaimInstanceIn(store, inst, principal.InstanceClaim{
		Session:  sessionClaim,
		OwnerPID: os.Getpid(),
		Cwd:      cwd,
		Mode:     "interactive",
		Task:     "interactive session " + card.ID,
	}, true)
	ctx := agentlaunch.InstanceContext{Instance: inst, Family: fam, Binding: l.Binding()}
	cleanup := func() {
		_, _ = store.Retire(inst.UUID, bus.InstanceMail)
		principal.ReleaseInstance(inst, sessionClaim)
	}
	return ctx, sessionClaim, cleanup
}
