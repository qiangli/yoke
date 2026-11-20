package chat

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

func TestChatSteerQueuesUntilReadyOrTimeout(t *testing.T) {
	for _, signal := range []string{"prompt", "room", "timeout"} {
		t.Run(signal, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			t.Setenv("BASHY_MB_DIR", t.TempDir())
			t.Setenv("BASHY_ROOM_DIR", t.TempDir())
			t.Setenv("BASHY_PRINCIPAL", "conductor")
			card := room.Card{ID: "weave-1", Mode: "weave", PID: os.Getpid(), CtlSock: "ctl", Caps: []string{SteerInboxPrefix + "test-run"}}
			if err := room.Join(card); err != nil {
				t.Fatal(err)
			}
			var attempts []string
			var firstAttempt time.Time
			oldSend, oldFirst := steerSend, steerInterruptFirst
			t.Cleanup(func() { steerSend, steerInterruptFirst = oldSend, oldFirst })
			steerInterruptFirst = func(string) bool { return false }
			steerSend = func(sock, frame string) error {
				attempts = append(attempts, frame)
				if len(attempts) == 1 {
					firstAttempt = time.Now()
					return errors.New("socket starting")
				}
				return nil
			}
			for _, text := range []string{"first line", "second line"} {
				cmd := newChatSteerCmd()
				var out bytes.Buffer
				cmd.SetErr(&out)
				cmd.SetArgs([]string{card.ID, text})
				if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "queued") {
					t.Fatalf("steer: %v %s", err, out.String())
				}
			}
			var warnings bytes.Buffer
			timeout := time.Minute
			if signal == "timeout" {
				timeout = 100 * time.Millisecond
			}
			start := time.Now()
			relay := startSteerRelay(card, 0, nil, 5*time.Millisecond, timeout, &warnings)
			t.Cleanup(relay.Close)
			if signal != "timeout" {
				relay.Write([]byte("loading\r\nTrust this folder?\r\n› 1. Trust and continue"))
				if err := room.Emit(room.Event{Type: room.EventStatus, Target: "different-worker", Body: "ready"}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(40 * time.Millisecond)
				if posts, _, _, err := bus.Unseen("test-run", 1); err != nil || len(posts) != 2 {
					t.Fatalf("startup queue = %v, %v", posts, err)
				}
				if signal == "prompt" {
					relay.GateAnswered(agentpty.GateVerdict{}, "")
					relay.Write([]byte("welcome\x1b[3;1H❯ "))
				} else {
					if err := room.Emit(room.Event{Type: room.EventStatus, Target: card.ID, Body: "ready"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			for bus.SeenSeq("test-run") < 2 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if bus.SeenSeq("test-run") != 2 {
				t.Fatal("queued lines not acknowledged")
			}
			// Join the writer before inspecting its in-memory trace.
			want := []string{agentpty.TextFrame("first line"), agentpty.TextFrame("first line"), agentpty.TextFrame("second line")}
			relay.Close()
			if signal == "timeout" {
				if firstAttempt.Sub(start) < timeout || strings.Count(warnings.String(), "warning:") != 1 {
					t.Fatalf("first attempt after %s, warnings %q", firstAttempt.Sub(start), warnings.String())
				}
			} else if warnings.Len() != 0 {
				t.Fatalf("unexpected warning: %s", &warnings)
			}
			if !slices.Equal(attempts, want) {
				t.Fatalf("delivery attempts = %q, want %q", attempts, want)
			}
		})
	}
}

// A fresh chat steer inbox starts at the board head: the relay's first read
// takes the tail path instead of a full-board scan, and a steer posted after
// the inbox exists is still delivered.
func TestNewSteerInboxStartsAtBoardHead(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "conductor")
	for i := 0; i < 3; i++ {
		if err := bus.PostMessage(bus.Post{To: "someone-else", From: "conductor", Body: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	addr := steerInbox(room.Card{Caps: []string{newSteerInboxCap()}})
	if seq, ok := bus.CursorSeq(addr); !ok || seq < 3 {
		t.Fatalf("cursor = %d, %v; want at the board head", seq, ok)
	}
	if err := bus.PostMessage(bus.Post{To: addr, From: "conductor", Body: "steer"}); err != nil {
		t.Fatal(err)
	}
	posts, _, _, err := bus.Unseen(addr, 1)
	if err != nil || len(posts) != 1 || posts[0].Body != "steer" {
		t.Fatalf("unseen = %+v, %v; want the one steer", posts, err)
	}
}

func TestStartedLineNamesTheWatchedInbox(t *testing.T) {
	if got, want := startedLine("claude", true), "chat: \"claude\" started — watching inbox claude (`bashy mb send claude \"...\"`)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := startedLine("claude", false); !strings.Contains(got, "inbox delivery off") {
		t.Fatalf("inbox off: %q", got)
	}
}
