//go:build !windows

package chat

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

func TestInteractiveTrustLayers(t *testing.T) {
	for _, mode := range []string{"preseed", "fallback", "no-trust", "attended", "queued"} {
		t.Run(mode, func(t *testing.T) {
			pinCatalog(t)
			isolateRoom(t)
			setTestHome(t, t.TempDir())
			t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
			t.Setenv("BASHY_CHAT_INBOX", "off")
			if mode == "queued" {
				t.Setenv("BASHY_CHAT_INBOX", "")
			}
			t.Setenv("BASHY_MB_DIR", t.TempDir())
			t.Setenv("BASHY_ROOM_DIR", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			root := t.TempDir()
			cwd := filepath.Join(root, "workspace")
			if err := os.Mkdir(cwd, 0o700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(cwd, alias); err != nil {
				t.Fatal(err)
			}
			trace := filepath.Join(root, "trace")
			t.Setenv("FAKE_TRUST_TRACE", trace)
			t.Setenv("FAKE_TRUST_MODE", mode)

			// The fake defaults to "No, exit". It accepts an Up arrow, redraws
			// with the affirmative option selected, then reads exactly one Enter.
			bin := filepath.Join(root, "claude")
			script := `#!/bin/sh
stty -echo -icanon -icrnl min 1 time 0
config="$CLAUDE_CONFIG_DIR/.claude.json"
if [ "$FAKE_TRUST_MODE" = preseed ]; then
  if grep -F "$(pwd -P)" "$config" && grep -F '"hasTrustDialogAccepted": true' "$config"; then
    echo PRESEEDED >> "$FAKE_TRUST_TRACE"
  else
    echo MISSING_PRESEED >> "$FAKE_TRUST_TRACE"
    exit 8
  fi
else
  printf 'Quick safety check: Do you trust the files in this folder?\r\n  1. Yes, I trust this folder\r\n❯ 2. No, exit\r\n'
  echo DIALOG >> "$FAKE_TRUST_TRACE"
  if [ "$FAKE_TRUST_MODE" = fallback ]; then
    key=$(dd bs=1 count=3 2>/dev/null)
    [ "$key" = "$(printf '\033[A')" ] || { echo WRONG_NAVIGATION >> "$FAKE_TRUST_TRACE"; exit 9; }
    sleep 0.05
    printf '\033[2J❯ 1. Yes, I trust this folder\r\n  2. No, exit\r\n'
    key=$(dd bs=1 count=1 2>/dev/null)
    [ "$key" = "$(printf '\r')" ] || { echo WRONG_CONFIRMATION >> "$FAKE_TRUST_TRACE"; exit 10; }
    echo TRUSTED >> "$FAKE_TRUST_TRACE"
  else
    if [ "$FAKE_TRUST_MODE" = queued ]; then stty min 0 time 25; else stty min 0 time 8; fi
    key=$(dd bs=1 count=1 2>/dev/null)
    [ -z "$key" ] || { echo INPUT_IN_DIALOG >> "$FAKE_TRUST_TRACE"; exit 11; }
    echo WAITED >> "$FAKE_TRUST_TRACE"
    exit
  fi
fi
stty icrnl
printf '\033[2J❯ \r\nstatus footer\r\n'
IFS= read -r line
printf 'OPENING:%s\n' "$line" >> "$FAKE_TRUST_TRACE"
`
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			tool, ok := newCatalog().Tool("claude")
			if !ok {
				t.Fatal("Claude tool seed missing")
			}
			tool.CLI.Managed = nil
			tool.CLI.Binary = bin
			tool.CLI.Launch.SteerExec = bin
			tool.CLI.Launch.Auth = fleet.ToolAuth{}
			if err := newCatalog().SaveTool(tool); err != nil {
				t.Fatal(err)
			}
			// Keep the fake tests fast and force the delivery timeout to pass
			// while the opt-out cases are still at their dialog.
			prior := defaultInteractiveDeliveryTiming
			defaultInteractiveDeliveryTiming = testDeliveryTiming()
			t.Cleanup(func() { defaultInteractiveDeliveryTiming = prior })

			var status bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			opts := InteractOptions{Cwd: alias, Prompt: "hello", Unattended: mode != "attended", Status: &status}
			queued := make(chan error, 1)
			if mode == "queued" {
				opts.Prompt, opts.NoTrust = "", true
				go func() {
					for {
						if _, err := os.Stat(trace); err == nil {
							break
						}
						select {
						case <-ctx.Done():
							queued <- ctx.Err()
							return
						case <-time.After(10 * time.Millisecond):
						}
					}
					card, _, _ := room.Find("claude")
					for _, target := range []string{steerInbox(card), "claude"} {
						if err := bus.PostMessage(bus.Post{From: "test-operator", To: target, Body: "pending"}); err != nil {
							queued <- err
							return
						}
						pending, _, _, err := bus.Unseen(target, 1)
						if err != nil || len(pending) == 0 {
							queued <- fmt.Errorf("test message missing for %q: %v", target, err)
							return
						}
					}
					queued <- nil
				}()
			}
			var code int
			var err error
			if mode == "no-trust" {
				cmd := NewChatCmd()
				cmd.SetErr(&status)
				cmd.SetArgs([]string{"-i", "--yolo", "--no-trust", "--agent", "claude", "--cwd", alias, "-m", "hello"})
				err = cmd.ExecuteContext(ctx)
			} else {
				code, err = Interact(ctx, "claude", opts)
			}
			if mode == "queued" {
				cancel()
				if err := <-queued; err != nil {
					t.Fatal(err)
				}
			}
			out, readErr := os.ReadFile(trace)
			if readErr != nil {
				t.Fatalf("fake did not launch: code=%d err=%v status=%s", code, err, &status)
			}
			switch mode {
			case "preseed", "fallback":
				want := "PRESEEDED\nOPENING:hello\n"
				if mode == "fallback" {
					want = "DIALOG\nTRUSTED\nOPENING:hello\n"
					if !strings.Contains(status.String(), "preseed did not clear it") {
						t.Fatalf("missing fallback notice: %s", &status)
					}
				}
				if err != nil || code != 0 || string(out) != want || !strings.Contains(status.String(), "hasTrustDialogAccepted=true") {
					t.Fatalf("trace=%q code=%d err=%v status=%s", out, code, err, &status)
				}
			case "queued":
				if err != nil || code != 0 || string(out) != "DIALOG\nWAITED\n" ||
					strings.Contains(status.String(), "delivering queued steers anyway") {
					t.Fatalf("trace=%q code=%d err=%v status=%s", out, code, err, &status)
				}
			default:
				if string(out) != "DIALOG\nWAITED\n" || err == nil || !strings.Contains(err.Error(), "exited before its instruction") {
					t.Fatalf("trace=%q code=%d err=%v status=%s", out, code, err, &status)
				}
				if _, err := os.Stat(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json")); !os.IsNotExist(err) {
					t.Fatalf("trust config changed with automation disabled: %v", err)
				}
			}
		})
	}
}
