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

// An ad hoc launch never holds the bare tool name: its id is the name plus the
// first group of the UUID its instance opens with, and its nick stays the name.
func TestJoinAdHocDerivesTheIDFromTheInstanceUUID(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	draws := []string{"7f3a9c1e-0000-4000-8000-000000000001"}
	stubInstanceUUIDs(t, draws)

	card := room.Card{ID: "claude", Nick: "claude", Tool: "claude", Mode: "interactive", PID: os.Getpid()}
	got, err := joinAdHoc(&card)
	if err != nil {
		t.Fatal(err)
	}
	if got != draws[0] {
		t.Fatalf("uuid = %q, want the one drawn", got)
	}
	if card.ID != "claude-7f3a9c1e" || card.Nick != "claude" || card.CtlSock == "" {
		t.Fatalf("got id=%q nick=%q sock=%q, want claude-7f3a9c1e, nick claude, its own socket", card.ID, card.Nick, card.CtlSock)
	}
	if _, ok, _ := room.Find("claude-7f3a9c1e"); !ok {
		t.Fatal("claude-7f3a9c1e not in the room")
	}
	if c, ok, _ := room.Find("claude"); !ok || c.ID != "claude-7f3a9c1e" {
		t.Fatalf("the base name does not reach the only live session: %+v ok=%v", c, ok)
	}
}

// A draw whose short form a live session already holds is not shared: the
// launch mints again.
func TestJoinAdHocMintsAgainWhenTheIDIsLive(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Skip("no sleep binary:", err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _, _ = other.Process.Wait() })
	if err := room.Join(room.Card{ID: "claude-7f3a9c1e", Mode: "interactive", PID: other.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	second := "0badc0de-0000-4000-8000-000000000002"
	stubInstanceUUIDs(t, []string{"7f3a9c1e-0000-4000-8000-000000000001", second})

	card := room.Card{ID: "claude", Nick: "claude", Mode: "interactive", PID: os.Getpid()}
	got, err := joinAdHoc(&card)
	if err != nil {
		t.Fatal(err)
	}
	if got != second || card.ID != "claude-0badc0de" {
		t.Fatalf("got uuid=%q id=%q, want the second draw and claude-0badc0de", got, card.ID)
	}
	if c, ok, _ := room.Find("claude"); ok {
		t.Fatalf("the base name is ambiguous with two live sessions, got %+v", c)
	}
}

// Attach by base name sees only this launch's own ad hoc sessions: same tool
// and binding, an id this package minted.
func TestLiveAdHocSessionsMatchesOnlyMintedIDs(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	l := Launch{Nick: "claude", Tool: "claude", ToolName: "claude"}
	seed := func(c room.Card) {
		t.Helper()
		c.Mode, c.PID = "interactive", os.Getpid()
		if err := room.Join(c); err != nil {
			t.Fatalf("seed %s: %v", c.ID, err)
		}
	}
	seed(room.Card{ID: "claude-helper", Tool: "claude"})                                // a named agent
	seed(room.Card{ID: "claude-opus-7f3a9c1e", Tool: "claude", Binding: "claude:opus"}) // another binding
	seed(room.Card{ID: "claude+2", Tool: "claude"})                                     // an older +N instance
	if _, n := liveAdHocSessions("claude", l); n != 0 {
		t.Fatalf("matched %d sessions, want none", n)
	}
	seed(room.Card{ID: "claude-7f3a9c1e", Tool: "claude"})
	if c, n := liveAdHocSessions("claude", l); n != 1 || c.ID != "claude-7f3a9c1e" {
		t.Fatalf("got %d (%s), want the one ad hoc session", n, c.ID)
	}
	seed(room.Card{ID: "claude-0badc0de", Tool: "claude"})
	if _, n := liveAdHocSessions("claude", l); n != 2 {
		t.Fatalf("matched %d sessions, want 2", n)
	}
}

func TestAdHocIDKeepsAModelInTheBase(t *testing.T) {
	if got := adHocID("claude-opus", "7F3A9C1E-0000-4000-8000-000000000001"); got != "claude-opus-7f3a9c1e" {
		t.Fatalf("id = %q", got)
	}
}

func stubInstanceUUIDs(t *testing.T, draws []string) {
	t.Helper()
	old := newInstanceUUID
	t.Cleanup(func() { newInstanceUUID = old })
	n := 0
	newInstanceUUID = func() string {
		if n >= len(draws) {
			t.Fatalf("drew %d UUIDs, only %d stubbed", n+1, len(draws))
		}
		n++
		return draws[n-1]
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
