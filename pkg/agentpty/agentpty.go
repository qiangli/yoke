// Package agentpty runs an agent CLI attached to a pseudo-terminal, with the
// watchdogs, the control channel, and the interactive-gate handling that a
// headless fleet needs.
//
// # Why a PTY at all
//
// Agent CLIs are TUIs. Handed a plain pipe they behave differently, and some of
// them stop dead: the first thing several ask is "do you trust this directory?",
// on a terminal, expecting a keystroke. A headless launcher that cannot answer
// that question does not get a slow agent — it gets an agent that produces
// nothing at all and then times out. Giving the process a controlling terminal
// it can talk to, and watching what it says, is what makes an unattended fleet
// possible.
//
// So this package does three things a plain exec cannot:
//
//   - Allocates a PTY, so the agent believes a human is present even when the
//     parent is an orchestrator's pipe.
//   - Watches the output for an interactive GATE — a trust prompt, an OAuth
//     redirect, a device code, a missing API key — and either clears it
//     (keystrokes on the PTY master) or escalates it to a human, rather than
//     letting the run stall silently until its idle timeout.
//   - Serves a control socket, so an orchestrator can STEER a running agent by
//     writing a line into it. That is how a chair tells a rambling participant
//     to get to the point without killing the turn.
//
// # Why it lives here and not in weave
//
// It was weave's, and weave is still its biggest consumer. It moved because
// `chat` needs it too — a meeting participant that hangs on a trust prompt is
// exactly as stuck as a weave worker — and `weave` already imports `chat`, so
// chat could not import weave without a cycle.
//
// This package deliberately knows nothing about weave's queue, chat's launcher,
// or meet's transcript. Everything host-specific is injected: the browser-login
// route, the escalation route, and the log filter are all function fields. That
// is what keeps `cmds/browser` and a work queue out of the import graph of a
// package that only wanted to run a subprocess.
package agentpty

import (
	"io"
	"time"
)

// Options are the tripwires and channels a supervised agent run gets.
//
// The zero value is a plain PTY run with no watchdogs and no control channel —
// safe, and what a short interactive turn wants. A long unattended run wants all
// of them.
type Options struct {
	// KillOnParentExit makes this supervised process group self-clean when its
	// caller disappears. Deliberate setpgid/setsid escape requires an OS
	// containment backend and is outside the portable procguard guarantee.
	// Unsupported platforms fail closed. Used by bounded Meet turns, not durable
	// worker sessions.
	KillOnParentExit bool

	// IdleTimeout kills the process tree when it stops WRITING for this long.
	// Useless against a runaway TUI whose spinner keeps emitting, which is what
	// MaxRuntime is for.
	IdleTimeout time.Duration

	// Activity reports structured progress that arrives outside the PTY (for
	// example, an agent's declared event file). Each signal resets IdleTimeout
	// exactly like a successful PTY write. The producer owns and may close it.
	Activity <-chan struct{}

	// MaxRuntime is a hard wall-clock ceiling that activity cannot reset. It is
	// measured against real elapsed time, not a monotonic timer, because a
	// monotonic timer PAUSES while the host sleeps — a run spanning an overnight
	// laptop suspend would otherwise never hit its ceiling.
	MaxRuntime time.Duration

	// MemLimitBytes kills the tree when its summed RSS exceeds this. The OOM
	// backstop: whatever leaks, the agent dies at its budget instead of taking
	// the machine down with it.
	MemLimitBytes int64

	// CtlSock is a unix socket path. Each line written to it becomes keystrokes
	// on the PTY master (trailing \r = Enter), which is both how an operator
	// steers a running agent and how a trust prompt gets cleared automatically.
	// Empty disables steering — and with it, automatic gate clearing.
	CtlSock string

	// OnStart is called after the child is started on its PTY and before any
	// sidecar reader begins draining the master. The argument is the real slave
	// device path, suitable for publishing as a terminal line through a symlink.
	// A non-nil error terminates the child and fails the run.
	OnStart func(ttyPath string) error

	// Capture forces the output to be captured even when the parent process is
	// itself a terminal.
	//
	// The default (false) hands a TTY parent's screen straight to the agent, so
	// a human at a keyboard sees its TUI and can type into it. That is right for
	// `weave attach` and wrong for anything that RECORDS the agent's answer: a
	// meeting turn is captured and written to a transcript, and the human is
	// watching through `meet observe`, not typing at the agent. Without this,
	// running such a turn from an interactive shell would paint the agent's TUI
	// over the operator's terminal and record nothing.
	Capture bool

	// Filter wraps the sink before the PTY's output is copied into it, returning
	// the writer to copy into and an optional flush to run at exit.
	//
	// It exists because consumers disagree about what the output IS. weave wants
	// an agent's stream-json decoded into a readable worker log; a meeting wants
	// the raw lines, exactly as written, to stream to whoever is watching. Both
	// are right, and neither belongs in here.
	//
	// nil passes the sink through untouched.
	Filter func(io.Writer) (io.Writer, func() error)
}

// filter applies Options.Filter, or passes through when there is none.
func (o Options) filter(w io.Writer) (io.Writer, func() error) {
	if o.Filter == nil {
		return w, nil
	}
	return o.Filter(w)
}
