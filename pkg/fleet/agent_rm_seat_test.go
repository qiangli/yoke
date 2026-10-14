package fleet

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func TestAgentRmSeat(t *testing.T) {
	roomDir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", roomDir)
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithoutCloudOverlay()}

	pid := os.Getpid()
	c := room.Card{
		ID:      "seat-100",
		Binding: "tool:model",
		Nick:    "AgentNick",
		Tool:    "tool",
		PID:     pid,
	}
	if err := room.Join(c); err != nil {
		t.Fatalf("room.Join: %v", err)
	}

	// 1. Remove by seat ID.
	out, err := runCmd(t, NewAgentsCmd(opts...), "rm", "--seat", "seat-100")
	if err != nil {
		t.Fatalf("agent rm --seat seat-100 failed: %v", err)
	}
	want := fmt.Sprintf("removed seat seat-100 (pid %d)\n", pid)
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if _, ok, _ := room.Find("seat-100"); ok {
		t.Fatal("seat-100 still found after rm --seat")
	}

	// 2. Remove by nick fallback.
	c2 := room.Card{
		ID:      "seat-200",
		Binding: "tool:model",
		Nick:    "AgentNick2",
		Tool:    "tool",
		PID:     pid,
	}
	if err := room.Join(c2); err != nil {
		t.Fatalf("room.Join: %v", err)
	}
	out, err = runCmd(t, NewAgentsCmd(opts...), "rm", "--seat", "AgentNick2")
	if err != nil {
		t.Fatalf("agent rm --seat AgentNick2 failed: %v", err)
	}
	want2 := fmt.Sprintf("removed seat AgentNick2 (pid %d)\n", pid)
	if out != want2 {
		t.Fatalf("got %q, want %q", out, want2)
	}
	if _, ok, _ := room.Find("seat-200"); ok {
		t.Fatal("seat-200 still found after rm --seat AgentNick2")
	}

	// 3. Not-found error.
	_, err = runCmd(t, NewAgentsCmd(opts...), "rm", "--seat", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent seat, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}
