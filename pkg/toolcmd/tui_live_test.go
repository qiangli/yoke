package toolcmd

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TestLiveTUIPlan drives the REAL claude TUI through runTUI: `/plan` is a
// built-in that exists only in the interactive UI (claude -p refuses it).
// Opt-in and billed: BASHY_TOOLCMD_LIVE=1. The command is declared inline, so
// the fleet store is only read (claude's steer_exec), never written.
func TestLiveTUIPlan(t *testing.T) {
	if os.Getenv("BASHY_TOOLCMD_LIVE") != "1" {
		t.Skip("live TUI smoke: set BASHY_TOOLCMD_LIVE=1 (spends a claude turn)")
	}
	// The test binary is not bashy: never hand it to the agent as its shell
	// (and never point ~/.bashy/shims at it).
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
	// claude's steer_exec carries --dangerously-skip-permissions and the launch
	// guard refuses it uncontained. The runner never loosens that gate; this
	// opt-in smoke accepts it explicitly for a plan-only turn in a temp dir.
	t.Setenv("BASHY_ALLOW_UNSAFE_AGENT_LAUNCH", "1")
	tool, ok := fleet.New().Tool("claude")
	if !ok {
		t.Skip("claude not in the fleet catalog")
	}
	cmd := fleet.ToolCommand{
		Name: "plan", Slash: "/plan {args}", Mode: fleet.ToolCommandTUI, Timeout: "4m", Quit: "/exit",
		Steps: []fleet.ToolCommandStep{{WaitIdle: "25s"}, {Key: "esc"}, {WaitIdle: "3s"}},
	}
	tool.Commands = []fleet.ToolCommand{cmd}
	res, err := Run(context.Background(), tool, cmd, "print hello (a one-line shell script); keep the plan to three lines", Options{Dir: t.TempDir(), Stdout: os.Stderr})
	t.Logf("outcome=%s duration=%s dir=%s\n--- turn ---\n%s", res.Outcome, res.Duration, res.Dir, res.Text)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeSuccess || res.Text == "" {
		t.Fatalf("result %+v", res)
	}
	time.Sleep(500 * time.Millisecond)
}
