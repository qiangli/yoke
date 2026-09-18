package meet

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// panicReader makes "returned without reading stdin" a deterministic property
// instead of a timing assertion. The old command reached repl and Read blocked
// indefinitely on the caller's still-open input stream.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("bounded meet open entered the interactive stdin loop")
}

// The exact lifecycle shape observed in production looked bounded but every
// bound was silently inert because --non-interactive was absent. It must fail
// before creating a room or touching stdin, so there is no parent process (or
// child turn) left to reap hours later.
func TestOpenRejectsExplicitBoundsWithoutNonInteractive(t *testing.T) {
	meetDir := t.TempDir()
	t.Setenv("BASHY_MEET_DIR", meetDir)
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	t.Setenv(meetDepthEnv, "")

	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(panicReader{})
	cmd.SetArgs([]string{"open", "--rounds", "1", "--max-turns", "2", "--turn-timeout", "20m"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("bounded flags without --non-interactive entered an unbounded REPL")
	}
	if !strings.Contains(err.Error(), "--non-interactive") ||
		!strings.Contains(err.Error(), "--rounds") {
		t.Fatalf("refusal must name the unattended-only flag and correction, got %v", err)
	}
	entries, readErr := os.ReadDir(meetDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("refused bounded open persisted room state: %+v", entries)
	}
}

// Per-turn and chaired-loop limits are meaningful in an interactive room: a
// later /round uses TurnTimeout and /chair uses MaxTurns/MaxStalls. Only the
// unattended-only --rounds/--yes flags trigger the mode refusal above.
func TestOpenAllowsInteractiveTurnAndChairBounds(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	t.Setenv(meetDepthEnv, "")

	cmd := NewMeetCmd()
	cmd.SetIn(strings.NewReader("")) // enter the REPL, then clean EOF
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"open", "--topic", "interactive bounds", "--participant", "codex",
		"--owner", "gemini", "--max-turns", "2", "--max-stalls", "1", "--turn-timeout", "20m"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("interactive turn/chair bounds: %v", err)
	}
}

// Explicit dry-run remains a safe preview even when the caller is inspecting
// bounded settings: it launches nothing and exits before the REPL by contract.
func TestOpenDryRunMayPreviewExplicitBounds(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	t.Setenv(meetDepthEnv, "")
	out, err := runMeet(t, "open", "--topic", "bounded preview", "--participant", "codex",
		"--owner", "gemini", "--rounds", "1", "--turn-timeout", "20m", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run preview: %v", err)
	}
	if !strings.Contains(out, "dry-run: no agents launched") {
		t.Fatalf("dry-run output = %q", out)
	}
}

func TestOpenRequiresARegisteredFacilitator(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	t.Setenv(meetDepthEnv, "")

	_, err := runMeet(t, "open", "--topic", "owned meeting", "--participant", "codex",
		"--secretary", "", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "owner (facilitator)") ||
		!strings.Contains(err.Error(), "bashy agent list") {
		t.Fatalf("ownerless meeting refusal = %v", err)
	}

	if _, err := runMeet(t, "open", "--topic", "owned meeting", "--participant", "codex",
		"--owner", "gemini", "--secretary", "", "--dry-run"); err != nil {
		t.Fatalf("registered facilitator was refused: %v", err)
	}
}

// A completed or abandoned room is terminal. Resume/repl must reject it before
// waiting on stdin; panicReader pins that no scanner read occurred.
func TestREPLRejectsTerminalRoomWithoutReadingStdin(t *testing.T) {
	for _, status := range []string{"closed", "abandoned"} {
		t.Run(status, func(t *testing.T) {
			cmd := NewMeetCmd()
			cmd.SetIn(panicReader{})
			cmd.SetOut(io.Discard)
			st := &State{ID: "terminal-room", Status: status}
			err := repl(cmd, st)
			if err == nil || !strings.Contains(err.Error(), status) {
				t.Fatalf("repl terminal status %q: %v", status, err)
			}
		})
	}
}
