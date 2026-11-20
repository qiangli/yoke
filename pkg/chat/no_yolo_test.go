package chat

import (
	"bytes"
	"strings"
	"testing"
)

// --no-yolo reaches the launcher as DenyUnsafe, the most specific setting.
func TestNoYoloReachesTheLauncher(t *testing.T) {
	if !toAgentLaunchOptions(Options{DenyUnsafe: true}).DenyUnsafe {
		t.Fatal("DenyUnsafe dropped on the way to agentlaunch")
	}
}

// --yolo and --no-yolo contradict each other: a usage error, never a silent pick.
func TestYoloAndNoYoloAreMutuallyExclusive(t *testing.T) {
	cmd := NewChatCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--agent", "claude", "--yolo", "--no-yolo", "-m", "hi", "--dry-run"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no-yolo") {
		t.Fatalf("want a mutual-exclusion error, got %v (%s)", err, out.String())
	}
}
