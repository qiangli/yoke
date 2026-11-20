package agentpty

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
)

// ErrDialogActive leaves automated input pending until the TUI shows its prompt.
var ErrDialogActive = errors.New("terminal is waiting at a dialog")

// GateEnterRefusalError reports that an explicit Enter (`chat steer --enter`)
// was refused because a recognized interactive gate is currently on screen.
type GateEnterRefusalError struct {
	Verdict GateVerdict
}

func (e *GateEnterRefusalError) Error() string {
	if e.Verdict.Signature != "" {
		return fmt.Sprintf("terminal is waiting at a %s gate (%q); inspect the dialog or pass --force to send Enter anyway", e.Verdict.Kind, e.Verdict.Signature)
	}
	return fmt.Sprintf("terminal is waiting at a %s gate; inspect the dialog or pass --force to send Enter anyway", e.Verdict.Kind)
}

func (e *GateEnterRefusalError) Unwrap() error { return ErrDialogActive }

// DialogGuard tracks the foreground TUI and controls its automated input.
// Human keystrokes still go directly to the PTY.
type DialogGuard struct {
	mu      sync.Mutex
	inputMu sync.Mutex
	pty     io.Writer
	tail    string
	dialog  bool

	// sendingFirst is true from the first chunk of a text frame in Send until
	// that frame's first submit '\r' is written, so TUI self-echo of the text
	// being typed cannot latch dialog mid-frame and drop its own submit '\r'.
	sendingFirst bool

	answer func(string)
	wanted string
	done   bool
	redraw string
}

// NewDialogGuard enables the trust fallback when answered is supplied. The
// callback receives the label confirmed on screen.
func NewDialogGuard(answered func(string)) *DialogGuard {
	return &DialogGuard{answer: answered}
}

func (g *DialogGuard) Clear() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dialog && VisibleDialogGate(g.tail).Kind == GateNone && g.done {
		g.dialog = false
	}
	return !g.dialog
}

// ActiveGate returns the recognized gate currently visible on screen, clearing
// any stale latch when no gate remains visible after initial startup.
func (g *DialogGuard) ActiveGate() GateVerdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := VisibleDialogGate(g.tail)
	if v.Kind == GateNone && g.done {
		g.dialog = false
	}
	return v
}

// Send serializes automated frames and checks the screen at every PTY write,
// including the delayed Enter in a text frame. Once a text frame's chunks begin
// writing, self-echo cannot orphan its first submit '\r'; the delayed 3s
// resubmit '\r' still checks g.dialog so a newly opened dialog is not answered.
func (g *DialogGuard) Send(frame string) error {
	g.inputMu.Lock()
	defer g.inputMu.Unlock()
	line := strings.TrimSuffix(strings.TrimSuffix(frame, "\n"), "\r")
	isText := line != "" && !strings.HasPrefix(line, "\x00R") && !strings.HasPrefix(line, "\x00F")
	if isText {
		g.mu.Lock()
		if g.dialog && VisibleDialogGate(g.tail).Kind == GateNone && g.done {
			g.dialog = false
		}
		blocked := g.dialog
		if !blocked {
			g.sendingFirst = true
		}
		g.mu.Unlock()
		if blocked {
			return ErrDialogActive
		}
		defer func() {
			g.mu.Lock()
			g.sendingFirst = false
			g.mu.Unlock()
		}()
	}
	return writePTYControlLine(g, line)
}

// SendEnter handles an explicit `chat steer --enter`. If a recognized gate is
// visible on screen and force is false, it refuses with the gate kind/signature
// instead of blindly pressing Enter on a trust/auth/approval dialog. When the
// latch is stale/self-echo or no recognized gate is visible (or force is true),
// it delivers a single '\r'.
func (g *DialogGuard) SendEnter(pty io.Writer, force bool) error {
	g.inputMu.Lock()
	defer g.inputMu.Unlock()
	g.mu.Lock()
	v := VisibleDialogGate(g.tail)
	if !force {
		if v.Kind != GateNone {
			g.mu.Unlock()
			return &GateEnterRefusalError{Verdict: v}
		}
		if g.dialog {
			// Stale latch with no recognized gate visible on screen.
			g.dialog = false
		}
	}
	dst := pty
	if dst == nil {
		dst = g.pty
	}
	if dst == nil {
		g.mu.Unlock()
		return io.ErrClosedPipe
	}
	g.done = true
	g.mu.Unlock()
	_, err := dst.Write([]byte{'\r'})
	return err
}

func (g *DialogGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	if g.dialog && !g.sendingFirst {
		g.mu.Unlock()
		return 0, ErrDialogActive
	}
	dst := g.pty
	if dst == nil {
		g.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	g.done = true // no trust automation after the first ordinary input
	if g.sendingFirst && string(p) == "\r" {
		g.sendingFirst = false
	}
	g.mu.Unlock()
	return dst.Write(p)
}

func (g *DialogGuard) controlLine(pty io.Writer, line string) error {
	if line+"\n" == EnterFrame(false) {
		return g.SendEnter(pty, false)
	}
	if line+"\n" == EnterFrame(true) {
		return g.SendEnter(pty, true)
	}
	if strings.HasPrefix(line, "\x00R") || strings.HasPrefix(line, "\x00F") {
		return writePTYControlLine(pty, line)
	}
	return g.Send(line)
}

func (g *DialogGuard) output(w io.Writer) io.Writer {
	return &dialogOutput{w: w, guard: g}
}

type dialogOutput struct {
	w     io.Writer
	guard *DialogGuard
}

func (o *dialogOutput) Write(p []byte) (int, error) {
	o.guard.observe(string(p))
	return o.w.Write(p)
}

var dialogPrompt = regexp.MustCompile(`^(?:❯|›|>)(?:$| (?:ask |type |try ).*)`)
var trustOption = regexp.MustCompile(`^([❯›>➤]\s*)?(?:[0-9]+[.)]\s+)?(.+)$`)

func isComposerReadyRow(low string) bool {
	return dialogPrompt.MatchString(low) ||
		strings.Contains(low, "? for shortcuts") ||
		strings.Contains(low, "tab to queue message")
}

func (g *DialogGuard) observe(chunk string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tail = appendDialogTail(g.tail, chunk)
	if g.sendingFirst {
		// While g.Send is actively typing a steer line before its first '\r',
		// output is the TUI echoing our own prose — never latch dialog on it.
		return
	}
	verdict := VisibleDialogGate(g.tail)
	// A normal input row (or Codex 0.171.0's diff-rendered composer footer)
	// releases the dialog latch once no gate remains in the newly drawn tail.
	fresh := strings.Split(screenEnd(chunk, trustDialogLines), "\n")
	for i, row := range fresh {
		low := strings.ToLower(ScreenText(row))
		if isComposerReadyRow(low) &&
			VisibleDialogGate(strings.Join(fresh[i:], "\n")).Kind == GateNone {
			g.tail, g.dialog = strings.Join(fresh[i:], "\n"), false
			return
		}
	}
	// Keep the dialog blocked across cursor-only redraws until a prompt is drawn.
	g.dialog = g.dialog || verdict.Kind != GateNone
	if g.answer == nil || g.done || !g.dialog {
		return
	}
	screen := screenEnd(g.tail, trustDialogLines)
	if g.wanted != "" {
		g.redraw = appendDialogTail(g.redraw, chunk)
		screen = screenEnd(g.redraw, trustDialogLines)
	} else if verdict.Kind != GateTrust {
		return
	}
	label, move := trustSelection(screen, g.wanted)
	switch {
	case label == "":
		return
	case move != "":
		if _, err := io.WriteString(g.pty, move); err == nil {
			g.wanted = label
		}
	default:
		g.done = true
		if _, err := io.WriteString(g.pty, "\r"); err == nil {
			g.answer(label)
		}
	}
}

func appendDialogTail(tail, chunk string) string {
	tail += chunk
	if i := strings.LastIndex(tail, "\x1b[2J"); i >= 0 {
		tail = tail[i+4:]
	}
	const limit = 8 << 10
	if len(tail) > limit {
		tail = tail[len(tail)-limit:]
	}
	return tail
}

// trustSelection verifies the labelled option and its visible cursor. A move
// has to be followed by a fresh draw showing that option selected.
func trustSelection(screen, wanted string) (label, move string) {
	target, selected := -1, -1
	var targetLabel, selectedLabel string
	lines := strings.Split(screen, "\n")
	for i, line := range lines {
		line = ScreenText(line)
		match := trustOption.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		name := strings.ToLower(match[2])
		affirmative := strings.HasPrefix(name, "yes, i trust") ||
			strings.HasPrefix(name, "yes, trust") ||
			strings.HasPrefix(name, "trust and continue") ||
			((strings.HasPrefix(name, "trust this workspace") ||
				strings.HasPrefix(name, "trust this folder")) && !strings.Contains(name, "?")) ||
			name == "yes, proceed" || name == "yes, continue"
		if affirmative && wanted == "" {
			if target != -1 {
				return "", ""
			}
			target, targetLabel = i, match[2]
		}
		if match[1] != "" {
			if selected != -1 && wanted == "" {
				return "", ""
			}
			selected, selectedLabel = i, match[2]
		}
	}
	if wanted != "" {
		if selectedLabel == wanted {
			return wanted, ""
		}
		return "", ""
	}
	switch {
	case target < 0 || selected < 0:
		return "", ""
	case selected == target:
		return targetLabel, ""
	case selected > target && selected-target <= 3:
		return targetLabel, strings.Repeat("\x1b[A", selected-target)
	case target > selected && target-selected <= 3:
		return targetLabel, strings.Repeat("\x1b[B", target-selected)
	default:
		return "", ""
	}
}
