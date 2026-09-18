package herald

import (
	"context"
	"testing"

	"github.com/qiangli/yoke/pkg/acp"
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
	runner := newChatRunner(context.Background(), "not-a-real-agent", 0, false, false)
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
