package toolcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
)

// tuiSession is the slice of chat.Session the TUI runner drives. It is an
// interface so unit tests script a fake instead of a PTY and a process; the
// real one is chatTUISession (tui_session.go).
type tuiSession interface {
	// Ready blocks until the session can hear a line (control channel bound,
	// TUI drawn and quiet).
	Ready(ctx context.Context) error
	// Say types a line (a metered turn).
	Say(text string) error
	// Key presses raw bytes at the TUI (agentpty.VerbatimFrame).
	Key(b []byte) error
	WaitIdle(ctx context.Context, quiet time.Duration) error
	// Turn returns what was written since the last Turn and marks the end.
	Turn() string
	// Output is the whole session so far.
	Output() string
	// QuitLine asks the tool to leave: line, or the tool's own graceful quit
	// when line is empty. Never metered.
	QuitLine(line string) error
	Live() bool
	// Close ends the session now and reaps the process tree. Idempotent.
	Close()
}

// startTUISession starts a steerable session; a seam for tests.
var startTUISession = func(ctx context.Context, agent string, opt chat.SessionOptions) (tuiSession, error) {
	s, err := chat.Start(ctx, agent, opt)
	if err != nil {
		// chat.Start tears down (abortStart) whatever it launched before
		// returning an error; there is nothing for the caller to close.
		return nil, err
	}
	return &chatTUISession{s: s}, nil
}

// Tunables (vars so tests run instantly).
var (
	// tuiInterruptSettle is how long a steer_interrupt TUI gets to cancel its
	// turn after the ESC before a say step is typed (chat steer uses the same 2s).
	tuiInterruptSettle = 2 * time.Second
	// tuiFinalQuiet is the settle before the capture when the last step was not
	// itself a wait_idle.
	tuiFinalQuiet = 30 * time.Second
	// tuiQuitGrace is how long a graceful quit gets before the session is closed.
	tuiQuitGrace = 5 * time.Second
	// tuiSanitize strips the PTY chrome from captured text.
	tuiSanitize = chat.SanitizeTurn
)

// tuiStep is one planned act against the session.
type tuiStep struct {
	kind  string // say | key | wait | esc (steer_interrupt ESC before a say)
	text  string // say: the line; key: the key name
	bytes []byte // key/esc: the payload
	quiet time.Duration
	mark  bool // take (and discard) Turn() before this say, so the capture starts here
}

func (s tuiStep) String() string {
	switch s.kind {
	case "say":
		return "say: " + s.text
	case "key", "esc":
		return fmt.Sprintf("key: %s (%x)", s.text, s.bytes)
	default:
		return "wait_idle: " + s.quiet.String()
	}
}

// planTUI renders the slash line and the declared steps into the acts the
// runner will perform, refusing an unknown key BEFORE any session exists.
func planTUI(tool fleet.Tool, cmd fleet.ToolCommand, slash string) ([]tuiStep, error) {
	interrupt := tool.CLI.Launch.SteerInterrupt
	esc, _ := keyBytes("esc")
	plan := []tuiStep{{kind: "say", text: slash, mark: true}}
	for i, st := range cmd.Steps {
		switch {
		case st.Say != "":
			// ESC-first is the tool's declared steering contract (steer_interrupt):
			// its TUI holds a typed line until the current turn ends.
			if interrupt {
				plan = append(plan, tuiStep{kind: "esc", text: "esc", bytes: esc})
			}
			plan = append(plan, tuiStep{kind: "say", text: st.Say, mark: true})
		case st.Key != "":
			b, err := keyBytes(st.Key)
			if err != nil {
				return nil, fmt.Errorf("step %d: %w", i, err)
			}
			plan = append(plan, tuiStep{kind: "key", text: strings.ToLower(strings.TrimSpace(st.Key)), bytes: b})
		case st.WaitIdle != "":
			d, err := time.ParseDuration(st.WaitIdle)
			if err != nil {
				return nil, fmt.Errorf("step %d: wait_idle %q: %w", i, st.WaitIdle, err)
			}
			plan = append(plan, tuiStep{kind: "wait", quiet: d})
		default:
			return nil, fmt.Errorf("step %d: empty", i)
		}
	}
	if plan[len(plan)-1].kind != "wait" {
		plan = append(plan, tuiStep{kind: "wait", quiet: tuiFinalQuiet})
	}
	return plan, nil
}

// quitLine resolves how the session ends: the declared line, else the tool's
// own graceful quit ("" to QuitLine) when it has one, else none (close only).
func quitLine(tool fleet.Tool, cmd fleet.ToolCommand) (line string, graceful bool) {
	if q := strings.TrimSpace(cmd.Quit); q != "" {
		return q, true
	}
	return "", tool.CLI.Launch.SupportsGracefulQuit
}

// runTUI runs a tui-mode command through a steered session (steer_exec):
// Say the slash line, run the steps, capture, quit, and always tear down.
func runTUI(ctx context.Context, tool fleet.Tool, cmd fleet.ToolCommand, args string, opts Options) (res Result, err error) {
	started := time.Now()
	slash := RenderSlash(cmd.Slash, args)
	res = Result{Tool: tool.Name, Command: cmd.Name, Mode: cmd.Mode, Slash: slash, Started: started}
	fail := func(outcome string, e error) (Result, error) {
		res.Outcome, res.Error = outcome, e.Error()
		res.Duration = time.Since(started).String()
		return res, e
	}

	plan, err := planTUI(tool, cmd, slash)
	if err != nil {
		return fail(OutcomeError, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, err))
	}
	quit, graceful := quitLine(tool, cmd)

	if opts.DryRun {
		for _, st := range plan {
			res.Steps = append(res.Steps, st.String())
		}
		switch {
		case quit != "":
			res.Steps = append(res.Steps, "quit: "+quit)
		case graceful:
			res.Steps = append(res.Steps, "quit: (tool default)")
		}
		res.Dir = opts.Dir
		res.Outcome = OutcomeDryRun
		return res, nil
	}

	// Workdir: in place, or a fresh temp dir kept only when it holds artifacts.
	dir := opts.Dir
	if dir == "" {
		dir, err = os.MkdirTemp("", "toolcmd-"+tool.Name+"-")
		if err != nil {
			return fail(OutcomeError, fmt.Errorf("toolcmd: workdir: %w", err))
		}
		if !strings.HasPrefix(cmd.Output, OutputFilePrefix) {
			defer os.RemoveAll(dir)
		}
	}
	// The tool sees the RESOLVED path (macOS: /var -> /private/var). A trust
	// preseed keyed by the unresolved one misses, the trust dialog opens, and
	// the slash line answers it (measured: claude exited on "No, exit").
	if real, rerr := filepath.EvalSymlinks(dir); rerr == nil {
		dir = real
	}
	res.Dir = dir

	timeout := CommandTimeout(cmd, opts)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	agent := strings.TrimSpace(opts.Agent)
	if agent == "" {
		agent = tool.Name
		if m := strings.TrimSpace(opts.Model); m != "" {
			agent += ":" + m
		}
	}
	sess, err := startTUISession(runCtx, agent, chat.SessionOptions{
		Catalog: opts.Catalog,
		Cwd:     dir,
		Stream:  opts.Stdout,
		// agentpty's own MaxRuntime is the backstop behind runCtx.
		Timeout: timeout + time.Minute,
		Mode:    "toolcmd",
		Task:    tool.Name + ":" + cmd.Name,
	})
	if err != nil {
		return fail(tuiOutcome(ctx, runCtx, OutcomeError), fmt.Errorf("toolcmd: %s:%s: start session: %w", tool.Name, cmd.Name, err))
	}

	// TEARDOWN, on every path. A graceful quit only when the command did not
	// time out or get cancelled (the context has already stopped the child then);
	// Close always, which cancels and reaps the process tree.
	quitOK := false
	defer func() {
		if quitOK && (quit != "" || graceful) && sess.Live() {
			if qerr := sess.QuitLine(quit); qerr == nil {
				deadline := time.Now().Add(tuiQuitGrace)
				for sess.Live() && time.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
		sess.Close()
	}()

	if err := sess.Ready(runCtx); err != nil {
		return fail(tuiOutcome(ctx, runCtx, OutcomeError), fmt.Errorf("toolcmd: %s:%s: session not ready: %w", tool.Name, cmd.Name, err))
	}
	for _, st := range plan {
		if runCtx.Err() != nil {
			return fail(tuiOutcome(ctx, runCtx, OutcomeError), fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, runCtx.Err()))
		}
		var serr error
		switch st.kind {
		case "say":
			if st.mark {
				_ = sess.Turn() // the capture starts at the last thing we said
			}
			serr = sess.Say(st.text)
		case "key", "esc":
			serr = sess.Key(st.bytes)
			if st.kind == "esc" && tuiInterruptSettle > 0 {
				select {
				case <-runCtx.Done():
				case <-time.After(tuiInterruptSettle):
				}
			}
		case "wait":
			serr = sess.WaitIdle(runCtx, st.quiet)
		}
		if serr != nil {
			return fail(tuiOutcome(ctx, runCtx, OutcomeError), fmt.Errorf("toolcmd: %s:%s: %s: %w", tool.Name, cmd.Name, st, serr))
		}
	}
	if runCtx.Err() != nil {
		return fail(tuiOutcome(ctx, runCtx, OutcomeError), fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, runCtx.Err()))
	}

	// Capture.
	turn := sess.Turn()
	switch {
	case cmd.Output == OutputTranscript:
		res.Text = tuiSanitize(sess.Output())
	default: // turn, and file:<glob> (the turn rides along with the artifacts)
		res.Text = tuiSanitize(turn)
	}
	if glob, ok := strings.CutPrefix(cmd.Output, OutputFilePrefix); ok {
		res.Artifacts, err = tuiArtifacts(dir, glob)
		if err != nil {
			return fail(OutcomeError, fmt.Errorf("toolcmd: %s:%s: collect %s: %w", tool.Name, cmd.Name, cmd.Output, err))
		}
	}
	quitOK = true
	res.Outcome = OutcomeSuccess
	res.Duration = time.Since(started).String()
	return res, nil
}

// tuiOutcome classifies a failure: the caller cancelled, the command's own
// timeout fired, or something else went wrong.
func tuiOutcome(parent, run context.Context, other string) string {
	switch {
	case parent.Err() != nil:
		return OutcomeCancelled
	case errors.Is(run.Err(), context.DeadlineExceeded):
		return OutcomeTimeout
	default:
		return other
	}
}

// tuiArtifacts returns the regular files under dir matching glob
// (relative to dir), sorted, as absolute paths.
func tuiArtifacts(dir, glob string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, glob))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

// keyBytes maps a step's key name to the bytes a terminal sends for it.
// Named keys only: this is a control channel, not a remote keyboard, and an
// unknown name is refused rather than guessed.
func keyBytes(name string) ([]byte, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "esc", "escape":
		return []byte{0x1b}, nil
	case "enter", "return":
		return []byte{'\r'}, nil
	case "tab":
		return []byte{'\t'}, nil
	case "shift-tab", "backtab":
		return []byte("\x1b[Z"), nil
	case "space":
		return []byte{' '}, nil
	case "backspace":
		return []byte{0x7f}, nil
	case "up":
		return []byte("\x1b[A"), nil
	case "down":
		return []byte("\x1b[B"), nil
	case "right":
		return []byte("\x1b[C"), nil
	case "left":
		return []byte("\x1b[D"), nil
	}
	if l, ok := strings.CutPrefix(n, "ctrl-"); ok && len(l) == 1 && l[0] >= 'a' && l[0] <= 'z' {
		return []byte{l[0] - 'a' + 1}, nil
	}
	return nil, fmt.Errorf("unknown key %q (want esc, enter, tab, shift-tab, space, backspace, up, down, left, right, ctrl-<letter>)", name)
}
