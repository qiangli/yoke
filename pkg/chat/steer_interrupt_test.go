package chat

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/room"
)

// A CLI that holds typed input until its turn ends (Muse Code 1.3) must be
// interrupted before a steer, or a mid-turn STOP lands after the work
// (agent-bench l4/t3-stop, 2026-09-28). Other CLIs get the line alone.
func TestDeliverSteerInterruptsFirstOnlyWhenTheToolNeedsIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // deliverSteer emits room events: keep them out of the host room
	var sent []string
	oldSend, oldFirst, oldSettle := steerSend, steerInterruptFirst, steerInterruptSettle
	t.Cleanup(func() { steerSend, steerInterruptFirst, steerInterruptSettle = oldSend, oldFirst, oldSettle })
	steerSend = func(sock, frame string) error { sent = append(sent, frame); return nil }
	steerInterruptFirst = func(tool string) bool { return tool == "muse" }
	steerInterruptSettle = time.Millisecond

	line := "STOP. Another lane owns m3-m6."
	if err := deliverSteer(room.Card{ID: "m", Tool: "muse", CtlSock: "x"}, line); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0] != agentpty.VerbatimFrame([]byte{0x1b}) || sent[1] != agentpty.TextFrame(line) {
		t.Fatalf("muse: want ESC then the line, got %q", sent)
	}

	sent = nil
	if err := deliverSteer(room.Card{ID: "c", Tool: "claude", CtlSock: "x"}, line); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != agentpty.TextFrame(line) {
		t.Fatalf("claude: want the line alone, got %q", sent)
	}
}

// The flag is read from the fleet registry: muse declares it, claude does not.
func TestSteerInterruptFirstReadsTheToolBinding(t *testing.T) {
	if !steerInterruptFirst("muse") {
		t.Error("muse: steer_interrupt not declared in its tool binding")
	}
	if steerInterruptFirst("claude") {
		t.Error("claude: steer_interrupt must not be set (its TUI takes a line mid-turn)")
	}
	if steerInterruptFirst("no-such-tool") {
		t.Error("an unknown tool must not be interrupted")
	}
}
