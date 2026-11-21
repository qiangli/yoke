package chat

import (
	"os"
	"os/exec"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

// A live agent is never started twice: the next launch takes name+2, then
// name+3, each with its own room key and control socket.
func TestJoinNextInstanceTakesFirstFreePlusSuffix(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Skip("no sleep binary:", err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _, _ = other.Process.Wait() })

	for _, id := range []string{"claude", "claude+2"} {
		if err := room.Join(room.Card{ID: id, Mode: "interactive", PID: other.Process.Pid}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	card := room.Card{ID: "claude", Mode: "interactive", PID: os.Getpid()}
	if err := joinNextInstance(&card); err != nil {
		t.Fatal(err)
	}
	if card.ID != "claude+3" || card.Nick != "claude+3" || card.CtlSock == "" {
		t.Fatalf("got id=%q nick=%q sock=%q, want claude+3 with its own socket", card.ID, card.Nick, card.CtlSock)
	}
	if got, ok, _ := room.Find("claude+3"); !ok || got.PID != os.Getpid() {
		t.Fatalf("claude+3 not held by this process: %+v ok=%v", got, ok)
	}
}

func TestHostVersionLabel(t *testing.T) {
	old := HostVersion
	t.Cleanup(func() { HostVersion = old })
	HostVersion = func() string { return "1.0.0-rc.39" }
	if got := hostVersionLabel(); got != "bashy v1.0.0-rc.39" {
		t.Fatalf("label = %q", got)
	}
	HostVersion = func() string { return "" }
	if got := hostVersionLabel(); got != "bashy dev" {
		t.Fatalf("label = %q", got)
	}
}
