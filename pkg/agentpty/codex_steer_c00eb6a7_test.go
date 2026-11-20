package agentpty

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// Story c00eb6a7: ordinary engineering prose containing "authenticated"
// ("authenticated console route", "authenticated address discovery") must not
// classify as an interactive GateHuman, while genuine authentication prompts
// still do.
func TestClassifyGateIgnoresAuthenticatedProse(t *testing.T) {
	for _, prose := range []string{
		"DECISION 31d1e09e answered: APPROVED - match form/notepad's explicit handoff (that needs an authenticated console route: out of scope).",
		"The live browser check remains with the conductor, and authenticated address discovery is deferred to Sprint 683.",
	} {
		if v := ClassifyGate(prose); v.Kind != GateNone {
			t.Fatalf("ClassifyGate(%q) = %+v, want GateNone", prose, v)
		}
	}

	for _, prompt := range []string{
		"Authentication required\r\n❯ 1. Sign in",
		"Please authenticate to continue",
		"Session expired — authenticate with `login`",
	} {
		if v := ClassifyGate(prompt); v.Kind == GateNone {
			t.Fatalf("ClassifyGate(%q) = GateNone, want auth gate", prompt)
		}
	}
}

type echoingTerminal struct {
	got   bytes.Buffer
	guard *DialogGuard
}

func (e *echoingTerminal) Write(p []byte) (int, error) {
	n, err := e.got.Write(p)
	// Simulate Codex echoing each typed chunk straight into its composer row
	// before steerEnterDelay elapses.
	if string(p) != "\r" {
		e.guard.observe("\x1b[17;1H› " + string(p))
	}
	return n, err
}

// Story c00eb6a7 (1b): even if a steer line's own prose contains a gate phrase
// ("please log in", "session expired"), TUI self-echo while Send is typing the
// chunks must NEVER latch g.dialog before that line's first submit '\r' lands.
func TestSteerSelfEchoNeverOrphansFirstSubmitEnter(t *testing.T) {
	oldEnter, oldResubmit := steerEnterDelay, steerResubmitDelay
	steerEnterDelay, steerResubmitDelay = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { steerEnterDelay, steerResubmitDelay = oldEnter, oldResubmit })

	guard := NewDialogGuard(nil)
	term := &echoingTerminal{guard: guard}
	guard.pty = term

	steer := "Conductor: fix the 'session expired — please log in' error message in viewer_test.go and send DONE."
	if err := guard.Send(TextFrame(steer)); err != nil {
		t.Fatalf("guard.Send failed on self-echoed gate prose: %v", err)
	}
	if !strings.HasPrefix(term.got.String(), steer+"\r") {
		t.Fatalf("terminal received %q, want %q followed by '\\r'", term.got.String(), steer)
	}
}

// Story c00eb6a7 (2): explicit `steer --enter` passes through when no
// recognized gate is visible (including clearing a stale latch), refuses with
// the gate kind/signature when a real trust/auth/approval gate IS on screen,
// and honors --force.
func TestDialogGuardExplicitEnterRefusesVisibleGateUnlessForced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		screen   string
		wantKind GateKind
	}{
		{"trust gate", fakeTrustDialog, GateTrust},
		{"auth gate", "Authentication required\r\n❯ 1. Sign in\r\n", GateHuman},
		{"approval gate", "Allow command `rm -rf build`?\r\n❯ 1. Allow once\r\n  2. Always allow\r\n", GateApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pty bytes.Buffer
			guard := NewDialogGuard(nil)
			guard.pty = &pty
			guard.observe(tc.screen)

			// Unforced --enter must refuse and name the gate without writing '\r'.
			rawLine := strings.TrimSuffix(EnterFrame(false), "\n")
			err := guard.controlLine(&pty, rawLine)
			if err == nil || !errors.Is(err, ErrDialogActive) {
				t.Fatalf("unforced --enter err = %v, want ErrDialogActive", err)
			}
			if !strings.Contains(err.Error(), string(tc.wantKind)) || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("refusal %q missing gate kind %q or --force hint", err.Error(), tc.wantKind)
			}
			if pty.Len() != 0 {
				t.Fatalf("refused --enter wrote %q to PTY", pty.String())
			}

			// Forced --enter writes a single '\r'.
			forceLine := strings.TrimSuffix(EnterFrame(true), "\n")
			if err := guard.controlLine(&pty, forceLine); err != nil {
				t.Fatalf("forced --enter failed: %v", err)
			}
			if pty.String() != "\r" {
				t.Fatalf("forced --enter wrote %q, want \"\\r\"", pty.String())
			}
		})
	}

	// Stale latch with no visible gate on screen: unforced --enter clears latch and sends '\r'.
	var pty bytes.Buffer
	guard := NewDialogGuard(nil)
	guard.pty = &pty
	guard.dialog = true
	guard.tail = "\x1b[21;1H  Worked for 2m 18s\x1b[24;1H› unsent steer text\x1b[27;3H? for shortcuts"
	if err := guard.controlLine(&pty, strings.TrimSuffix(EnterFrame(false), "\n")); err != nil {
		t.Fatalf("stale-latch --enter failed: %v", err)
	}
	if pty.String() != "\r" || !guard.Clear() {
		t.Fatalf("stale-latch --enter pty=%q clear=%v, want '\\r' and unlatched", pty.String(), guard.Clear())
	}
}

// Story c00eb6a7 (3): Codex 0.171.0 diff-renders turn completion by updating
// the footer (`← for agents · ? for shortcuts`) and cursor `\x1b[24;3H` without
// re-printing the `›` glyph; DialogGuard must unlatch on that footer.
func TestDialogGuardUnlatchesOnCodexDiffRenderedFooter(t *testing.T) {
	var pty bytes.Buffer
	guard := NewDialogGuard(nil)
	guard.pty = &pty
	guard.observe("Authentication required\r\n❯ 1. Sign in\r\n")
	if guard.Clear() {
		t.Fatal("expected guard latched on auth dialog")
	}

	// Codex redraws clean screen with diff-rendered composer footer (no '›' glyph).
	guard.observe("\x1b[2J\x1b[21;1H  Worked for 2m 18s • 12:30 PM\x1b[27;3H← for agents · ? for shortcuts\x1b[24;3H\x1b[?25h")
	if !guard.Clear() {
		t.Fatal("Codex diff-rendered idle footer did not unlatch DialogGuard")
	}
}
