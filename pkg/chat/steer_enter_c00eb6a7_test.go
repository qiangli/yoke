package chat

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/room"
)

// Story c00eb6a7: `bashy chat steer <id> --enter` refuses with the gate name
// and `--force` guidance when a recognized trust/auth/approval gate is visible
// in the session log, passes through when no gate is visible (or after the gate
// cleared), and sends EnterFrame(true) when `--force` is given.
func TestChatSteerEnterRefusesVisibleGateUnlessForced(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())

	logPath := filepath.Join(t.TempDir(), "codex-session.log")
	if err := os.WriteFile(logPath, []byte(
		"Folder access\r\nTrust this folder? Continue only if you trust these files.\r\n› 1. Trust and continue\r\n  2. Back\r\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := room.Join(room.Card{
		ID:      "codex-s576",
		Nick:    "codex-s576",
		Tool:    "codex",
		Mode:    "interactive",
		PID:     os.Getpid(),
		CtlSock: "/tmp/fake-codex-s576.sock",
		LogPath: logPath,
	}); err != nil {
		t.Fatal(err)
	}

	var sentFrames []string
	prevSend := steerSend
	steerSend = func(_ string, frame string) error {
		sentFrames = append(sentFrames, frame)
		return nil
	}
	t.Cleanup(func() { steerSend = prevSend })

	runSteer := func(args ...string) error {
		cmd := newChatSteerCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		return cmd.Execute()
	}

	// 1. Unforced --enter while trust gate is visible: refused, zero frames sent.
	err := runSteer("codex-s576", "--enter")
	if err == nil {
		t.Fatal("expected refusal when trust gate is visible on screen")
	}
	for _, want := range []string{"trust", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q missing %q", err.Error(), want)
		}
	}
	if len(sentFrames) != 0 {
		t.Fatalf("refused --enter sent frames: %v", sentFrames)
	}

	// 2. --force without --enter is rejected as invalid usage.
	if err := runSteer("codex-s576", "--force", "some text"); err == nil || !strings.Contains(err.Error(), "--force only applies to --enter") {
		t.Fatalf("--force without --enter err = %v", err)
	}

	// 3. --enter --force sends EnterFrame(true).
	if err := runSteer("codex-s576", "--enter", "--force"); err != nil {
		t.Fatalf("--enter --force failed: %v", err)
	}
	if len(sentFrames) != 1 || sentFrames[0] != agentpty.EnterFrame(true) {
		t.Fatalf("sent frames = %q, want [%q]", sentFrames, agentpty.EnterFrame(true))
	}

	// 4. Once screen redraws to Codex composer (even containing "authenticated" prose!),
	//    unforced --enter succeeds with EnterFrame(false).
	sentFrames = nil
	if err := os.WriteFile(logPath, []byte(
		"\x1b[2JWorked for 2m • authenticated console route deferred\r\n› Conductor: DECISION 31d1e09e\r\n← for agents · ? for shortcuts\r\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSteer("codex-s576", "--enter"); err != nil {
		t.Fatalf("unforced --enter on composer screen failed: %v", err)
	}
	if len(sentFrames) != 1 || sentFrames[0] != agentpty.EnterFrame(false) {
		t.Fatalf("sent frames = %q, want [%q]", sentFrames, agentpty.EnterFrame(false))
	}
}
