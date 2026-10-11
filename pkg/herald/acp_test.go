package herald

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/acp"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

func TestHeraldRegistersACPSubcommand(t *testing.T) {
	cmd, _, err := NewHeraldCmd().Find([]string{"acp"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Name() != "acp" {
		t.Fatalf("Find(acp) = %v, want acp subcommand", cmd)
	}
}

func TestPromptTextPreservesBlockOrder(t *testing.T) {
	got := promptText([]acp.ContentBlock{
		acp.TextBlock("first"),
		{},
		acp.TextBlock("second"),
	})
	if got != "first\nsecond" {
		t.Fatalf("promptText = %q, want %q", got, "first\nsecond")
	}
}

func TestChatRunnerRefusesEmptyPromptWithoutLaunching(t *testing.T) {
	runner := newChatRunner(context.Background(), "not-a-real-agent", 0, false, false, "")
	got, err := runner.Run(context.Background(), acp.TurnRequest{
		SessionID: "session-1",
		Prompt:    []acp.ContentBlock{{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != acp.StopReasonRefusal {
		t.Fatalf("StopReason = %q, want refusal", got.StopReason)
	}
}

func TestValidateACPSandbox(t *testing.T) {
	for _, tc := range []struct {
		sandbox  string
		readOnly bool
		ok       bool
	}{
		{"", false, true},
		{"", true, true},
		{"workspace-write", false, true},
		{"workspace-write", true, false},
		{"danger-full-access", false, false},
		{"read-only", false, false},
	} {
		err := validateACPSandbox(tc.sandbox, tc.readOnly)
		if (err == nil) != tc.ok {
			t.Errorf("validateACPSandbox(%q, %v) = %v, want ok=%v", tc.sandbox, tc.readOnly, err, tc.ok)
		}
	}
}

func TestACPCommandRejectsIncompatibleSandbox(t *testing.T) {
	for _, args := range [][]string{
		{"--sandbox", "workspace-write", "--read-only"},
		{"--sandbox", "danger-full-access"},
	} {
		cmd := newACPCmd()
		cmd.SetArgs(args)
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		if err := cmd.Execute(); err == nil {
			t.Errorf("acp %v: want an error", args)
		}
	}
}

// The real ACP runner starts Codex under the requested safe profile: the
// recorded argv carries workspace-write and neither bypass form.
func TestACPRunnerLaunchesCodexWorkspaceWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a pty")
	}
	fleettest.Ring(t)
	bin := t.TempDir()
	rec := filepath.Join(t.TempDir(), "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + rec + "'\nexec sleep 4\n"
	fake := filepath.Join(bin, "codex")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The operator override keeps the managed pin from being downloaded.
	t.Setenv("BASHY_TOOL_BINARY_CODEX", fake)
	t.Setenv("BASHY_ACP", "")

	runner := newChatRunner(context.Background(), "codex", 3*time.Second, false, false, "workspace-write")
	defer runner.Close()
	_, runErr := runner.Run(context.Background(), acp.TurnRequest{
		SessionID: "s1",
		Cwd:       t.TempDir(),
		Prompt:    []acp.ContentBlock{acp.TextBlock("say PONG")},
	})

	raw, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("fake codex never launched: %v (run: %v)", err, runErr)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	joined := " " + strings.Join(args, " ") + " "
	if !strings.Contains(joined, " --sandbox workspace-write ") {
		t.Fatalf("argv lacks --sandbox workspace-write: %q", args)
	}
	for _, bad := range []string{"--dangerously-bypass-approvals-and-sandbox", "--yolo", "danger-full-access"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("argv carries %s: %q", bad, args)
		}
	}
}
