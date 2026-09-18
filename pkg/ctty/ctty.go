// Package ctty reaches the HUMAN OPERATOR from a process whose stdio does not
// belong to them.
//
// Every prompt helper in this tree — and in most programs — asks its question on
// stdin/stderr and assumes a person is on the other end. Under an agentic CLI
// (Claude Code, codex, agy, opencode) that assumption is false in a specific and
// damaging way: stdin and stdout are PIPES owned by the harness. A prompt written
// there is never seen by the human, and a value read from there is not the
// human's answer. The prompt in pkg/secrets is exactly this bug — it gates on
// `c.InOrStdin()` being a terminal, which under a harness it never is, so it
// silently falls through to reading the agent's already-closed stdin and reports
// "refusing to store an empty value".
//
// So the question this package answers is not "is stdin a terminal" but:
//
//	WHICH CHANNEL, IF ANY, REACHES THE PERSON WHO INVOKED THIS COMMAND?
//
// There are three, tried in order, and none of them is stdio:
//
//  1. The CONTROLLING TERMINAL (/dev/tty, CONIN$/CONOUT$) — the process's own
//     terminal, which survives stdio redirection. Available when a human ran the
//     command in a shell. Frequently NOT available under a harness: measured on
//     macOS, Claude Code setsid's its children, so /dev/tty returns ENXIO. bashy's
//     own agent launcher does the same thing deliberately (pkg/chat/proctree_unix.go).
//
//  2. A GUI ASKPASS helper (osascript / pinentry / zenity / kdialog / PowerShell) —
//     the SSH_ASKPASS pattern. This one DOES work from a setsid'd, tty-less child,
//     because it talks to the window server rather than to a terminal.
//
//  3. Nothing here — the caller falls back to an out-of-band rendezvous
//     (see pkg/ask), which prints an instruction the human can act on elsewhere.
//
// Two rules run through the whole package, and both exist because the failure
// they prevent is SILENT:
//
//   - ATTENDED, not merely PRESENT. A GUI existing on this machine does not mean
//     the person who typed the command can see it. Over SSH to a Mac, osascript
//     renders the dialog on the REMOTE machine's physical screen, returns success,
//     and the caller blocks forever on a prompt nobody will ever look at. That is
//     strictly worse than declining the rung, so when attendedness is uncertain we
//     decline. Falling through is always recoverable; a dialog on an unattended
//     screen is not.
//
//   - POSITIVE EVIDENCE OF AN ANSWER. `osascript ... giving up after 2` exits 0
//     and prints "gave up:true". A helper that trusts the exit code turns "nobody
//     was there" into a successful empty answer — a success state reached by the
//     ABSENCE of a signal, which is the one shape this project's evidence
//     invariant forbids outright. Every helper here parses its result and returns
//     an error unless it holds an affirmative confirmation AND a non-empty value.
package ctty

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

var (
	// ErrNoTTY means this process has no controlling terminal. Under an agentic
	// harness this is the common case, not an exotic one.
	ErrNoTTY = errors.New("ctty: no controlling terminal")

	// ErrNotForeground means we hold a controlling terminal but are not its
	// foreground process group, so another program is reading those keystrokes.
	// Prompting anyway would race the human's input against that program and
	// corrupt its display.
	ErrNotForeground = errors.New("ctty: not the foreground process group on the controlling terminal")

	// ErrNoGUI means no attended GUI askpass channel is available here.
	ErrNoGUI = errors.New("ctty: no attended GUI available")

	// ErrDeclined means the human was reached and said no — cancelled the dialog
	// or dismissed it. Distinct from ErrNoGUI: the channel worked, the answer was
	// "no". A caller must NOT fall through to another channel on this error, or
	// declining once would just summon a second prompt.
	ErrDeclined = errors.New("ctty: cancelled by the operator")

	// ErrTimeout means the prompt was displayed and nobody answered in time.
	// Like ErrDeclined this is a real outcome, not a channel failure.
	ErrTimeout = errors.New("ctty: timed out waiting for the operator")
)

// Request is one question for the human. The same shape drives every channel, so
// a tty prompt and a GUI dialog cannot drift apart in what they disclose.
type Request struct {
	// Frame is the trustworthy chrome the CALLER renders — who is asking, from
	// where, and where the answer will go. It is shown above Prompt and must not
	// contain caller-controlled text that has not been sanitized.
	Frame string
	// Prompt is the one-line question ("Value for GH_PAT"). When the requester is
	// an agent this text is UNTRUSTED and must already be sanitized by the caller.
	Prompt string
	// Title names the dialog window on GUI channels; ignored on a tty.
	Title string
	// Hidden disables echo (a secret) rather than showing what is typed.
	Hidden bool
	// Timeout bounds how long the human has. Zero means the channel's default.
	Timeout time.Duration
}

// text renders the frame and prompt as one block for a channel that shows all of
// it at once (a GUI dialog). The tty channel prints the frame itself and keeps
// the prompt on the input line, so it does not use this.
func (r Request) text() string {
	var b strings.Builder
	if f := strings.TrimRight(r.Frame, "\n"); f != "" {
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	b.WriteString(r.Prompt)
	return b.String()
}

func (r Request) title() string {
	if t := strings.TrimSpace(r.Title); t != "" {
		return t
	}
	return "bashy"
}

func (r Request) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 2 * time.Minute
}

// Channel names a way of reaching the human. It is a string rather than an enum
// so it round-trips through --channel, JSON, and env without a mapping table.
type Channel string

const (
	ChannelAuto       Channel = "auto"
	ChannelTTY        Channel = "tty"
	ChannelGUI        Channel = "gui"
	ChannelRendezvous Channel = "rendezvous"
	ChannelHandler    Channel = "handler"
)

// HandlerEnv, when set by a first-party harness, means "I will collect this input
// in my own UI; do not touch the terminal and do not create a rendezvous". It is
// read here (rather than in pkg/ask) so the decision function stays one place.
const HandlerEnv = "BASHY_ASK_HANDLER"

// AskpassEnv names an explicit askpass helper, honoured before autodetection and
// before the attendedness test — an operator who names a helper has asserted that
// it reaches them, and we do not second-guess that.
const AskpassEnv = "BASHY_ASKPASS"

// Probe is the observable state the channel decision is made from. It is a struct
// rather than a set of direct syscalls so ChooseChannel stays a PURE FUNCTION and
// can be exhaustively table-tested without a terminal, a window server, or an SSH
// session — which is the only way the remote-execution cases (the ones that would
// otherwise hang forever, unseen) get covered at all.
type Probe struct {
	// Requested is the operator's --channel, or ChannelAuto.
	Requested Channel
	// HandlerSet reports a first-party harness handler (HandlerEnv).
	HandlerSet bool
	// TTY reports that the controlling terminal is open-able AND we are its
	// foreground process group. Both, because either alone is insufficient.
	TTY bool
	// GUI reports that an ATTENDED GUI askpass helper is available — a helper
	// binary exists and the session is one the invoking human can see.
	GUI bool
}

// ChooseChannel picks the rung to try. Pure; see Probe.
//
// Order is tty → gui → rendezvous, and it is not arbitrary. The controlling
// terminal is first because when it exists the human is already looking at it and
// the interaction costs them nothing. The GUI is second because it reaches a human
// who is present at the machine but whose terminal is owned by a harness. The
// rendezvous is last because it is the only rung that always works and the only
// one that asks the human to go somewhere else.
//
// An explicit Requested channel is honoured verbatim, including when the probe
// says it will not work: an operator debugging a channel needs to be able to force
// it and see the real failure, not be silently rerouted.
func ChooseChannel(p Probe) Channel {
	if p.Requested != "" && p.Requested != ChannelAuto {
		return p.Requested
	}
	if p.HandlerSet {
		return ChannelHandler
	}
	if p.TTY {
		return ChannelTTY
	}
	if p.GUI {
		return ChannelGUI
	}
	return ChannelRendezvous
}

// CurrentProbe observes this process. Kept separate from ChooseChannel so the
// decision logic stays testable and only this thin layer touches the world.
func CurrentProbe(requested Channel) Probe {
	return Probe{
		Requested:  requested,
		HandlerSet: strings.TrimSpace(os.Getenv(HandlerEnv)) != "",
		TTY:        TTYUsable(),
		GUI:        GUIAvailable(),
	}
}

// TTYUsable reports whether the controlling terminal is both reachable and ours
// to read from.
func TTYUsable() bool {
	t, err := Open()
	if err != nil {
		return false
	}
	_ = t.Close()
	return true
}

// Ask puts the request to the human over the chosen channel.
//
// It returns ErrNoTTY/ErrNoGUI when the channel is unavailable (the caller should
// try the next rung) and ErrDeclined/ErrTimeout when the human was reached and did
// not answer (the caller must NOT retry on another channel — see ErrDeclined).
func Ask(ch Channel, req Request) ([]byte, error) {
	switch ch {
	case ChannelTTY:
		return askTTY(req)
	case ChannelGUI:
		return AskGUI(req)
	default:
		return nil, fmt.Errorf("ctty: channel %q cannot be served here", ch)
	}
}

// askTTY renders the frame on the controlling terminal and reads the answer there.
func askTTY(req Request) ([]byte, error) {
	t, err := Open()
	if err != nil {
		return nil, err
	}
	defer t.Close()

	if f := strings.TrimRight(req.Frame, "\n"); f != "" {
		if _, err := fmt.Fprintln(t, f); err != nil {
			return nil, err
		}
	}
	if req.Hidden {
		return t.ReadSecret(req.Prompt)
	}
	return t.ReadLine(req.Prompt)
}
