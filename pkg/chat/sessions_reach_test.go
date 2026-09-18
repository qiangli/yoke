package chat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

// A LIVE SESSION IS NOT NECESSARILY A REACHABLE ONE, and showing them
// identically is how somebody attaches to a session that will never answer —
// which reads as a hang rather than as a missing capability.
func TestReachLabel_DistinguishesSteerableFromReadOnly(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "a.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if got := reachLabel(room.Card{CtlSock: sock}); got != "steerable" {
		t.Errorf("a card with a present socket = %q, want steerable", got)
	}
	// Never had one: launched on a path that does not open a control channel.
	if got := reachLabel(room.Card{}); got != "log-only" {
		t.Errorf("a card with no socket = %q, want log-only", got)
	}
	// Advertises one that is gone. The process is alive — Members prunes dead
	// PIDs — so this is a session that LOST its control channel, which is a
	// different problem from never having had one and worth saying so.
	if got := reachLabel(room.Card{CtlSock: filepath.Join(t.TempDir(), "absent.sock")}); got != "no-ctl" {
		t.Errorf("a card whose socket vanished = %q, want no-ctl", got)
	}
}

// The footer explaining read-only rows must appear only when one is present —
// advice that is always shown is advice nobody reads.
func TestUnreachable_OnlyWhenSomethingIsReadOnly(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "b.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if unreachable([]room.Card{{CtlSock: sock}}) {
		t.Error("an all-steerable list must not print the read-only explanation")
	}
	if !unreachable([]room.Card{{CtlSock: sock}, {}}) {
		t.Error("a list containing a log-only session must explain it")
	}
}
