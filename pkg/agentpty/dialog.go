package agentpty

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
)

// ErrDialogActive leaves automated input pending until the TUI shows its prompt.
var ErrDialogActive = errors.New("terminal is waiting at a dialog")

// DialogGuard tracks the foreground TUI and controls its automated input.
// Human keystrokes still go directly to the PTY.
type DialogGuard struct {
	mu      sync.Mutex
	inputMu sync.Mutex
	pty     io.Writer
	tail    string
	dialog  bool

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
	return !g.dialog
}

// Send serializes automated frames and checks the screen at every PTY write,
// including the delayed Enter in a text frame.
func (g *DialogGuard) Send(frame string) error {
	g.inputMu.Lock()
	defer g.inputMu.Unlock()
	return writePTYControlLine(g, strings.TrimSuffix(strings.TrimSuffix(frame, "\n"), "\r"))
}

func (g *DialogGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dialog {
		return 0, ErrDialogActive
	}
	if g.pty == nil {
		return 0, io.ErrClosedPipe
	}
	g.done = true // no trust automation after the first ordinary input
	return g.pty.Write(p)
}

func (g *DialogGuard) controlLine(pty io.Writer, line string) error {
	if strings.HasPrefix(line, "\x00R") && line+"\n" != VerbatimFrame([]byte{'\r'}) {
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

func (g *DialogGuard) observe(chunk string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tail = appendDialogTail(g.tail, chunk)
	screen := screenEnd(g.tail, trustDialogLines)
	verdict := ClassifyGate(screen)
	// A normal input row may be followed by a status footer. Consider only
	// newly drawn rows, so an old prompt cannot release a newer dialog.
	fresh := strings.Split(screenEnd(chunk, trustDialogLines), "\n")
	for i, row := range fresh {
		if dialogPrompt.MatchString(strings.ToLower(ScreenText(row))) &&
			ClassifyGate(strings.Join(fresh[i:], "\n")).Kind == GateNone {
			g.tail, g.dialog = strings.Join(fresh[i:], "\n"), false
			return
		}
	}
	// Keep the dialog blocked across cursor-only redraws until a prompt is drawn.
	g.dialog = g.dialog || verdict.Kind != GateNone
	if g.answer == nil || g.done || !g.dialog {
		return
	}
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
