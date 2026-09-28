package chat

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/fleet"
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

// The flag is read from the fleet registry: a tool that declares
// steer_interrupt gets it, one that does not (and an unknown tool) does not.
// No baseline tool declares it today — Muse needed a second Enter, not an ESC
// (agentpty steerResubmitDelay) — so the test registers its own.
func TestSteerInterruptFirstReadsTheToolBinding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	prev := newCatalog
	newCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { newCatalog = prev })
	for name, flag := range map[string]bool{"holdsinput": true, "plaintui": false} {
		tool := fleet.Tool{Name: name, Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{
			Binary: "true",
			Launch: fleet.ToolLaunch{Exec: "true {prompt}", SteerInterrupt: flag},
		}}
		if err := newCatalog().SaveTool(tool); err != nil {
			t.Fatalf("SaveTool %s: %v", name, err)
		}
	}
	if !steerInterruptFirst("holdsinput") {
		t.Error("holdsinput declares steer_interrupt but was not interrupted")
	}
	if steerInterruptFirst("plaintui") {
		t.Error("plaintui does not declare steer_interrupt but would be interrupted")
	}
	if steerInterruptFirst("no-such-tool") {
		t.Error("an unknown tool must not be interrupted")
	}
}
