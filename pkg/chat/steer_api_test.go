package chat

import (
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

func steerAPIEnv(t *testing.T) *[]string {
	t.Helper()
	setTestHome(t, t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "conductor")
	var frames []string
	oldSend, oldFirst := steerSend, steerInterruptFirst
	t.Cleanup(func() { steerSend, steerInterruptFirst = oldSend, oldFirst })
	steerInterruptFirst = func(string) bool { return false }
	steerSend = func(sock, frame string) error {
		frames = append(frames, sock+"|"+frame)
		return nil
	}
	return &frames
}

func steerEvents(t *testing.T, target string) []room.Event {
	t.Helper()
	events, err := room.Timeline(100)
	if err != nil {
		t.Fatal(err)
	}
	var out []room.Event
	for _, e := range events {
		if e.Type == room.EventSteer && e.Target == target {
			out = append(out, e)
		}
	}
	return out
}

// Steer is `chat steer` for a caller that is not the command. A session with a
// steer inbox has the line queued there under the sender's name, and nothing
// is typed until the session's own relay drains it.
func TestSteerQueuesOnTheSessionsInboxAsItsSender(t *testing.T) {
	frames := steerAPIEnv(t)
	card := room.Card{ID: "codex", Mode: "interactive", PID: os.Getpid(), CtlSock: "ctl", Caps: []string{SteerInboxPrefix + "steer-codex"}}
	if err := room.Join(card); err != nil {
		t.Fatal(err)
	}
	rec, err := Steer("codex", "alice", "look at tab 7")
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if rec.ID != "codex" || !rec.Queued || rec.Headless {
		t.Fatalf("receipt = %+v, want queued for a session with a socket", rec)
	}
	posts, _, _, err := bus.Unseen("steer-codex", 10)
	if err != nil || len(posts) != 1 {
		t.Fatalf("steer inbox = %v, %v; want the one line", posts, err)
	}
	if p := posts[0]; p.From != "alice" || p.Body != "look at tab 7" || p.Topic != "codex" {
		t.Fatalf("post = %+v", p)
	}
	if len(*frames) != 0 {
		t.Fatalf("a queued steer was typed at once: %q", *frames)
	}

	// No sender named: this process's principal, as the command signs it.
	if _, err := Steer("codex", "", "second"); err != nil {
		t.Fatal(err)
	}
	if posts, _, _, _ := bus.Unseen("steer-codex", 10); len(posts) != 2 || posts[1].From != "conductor" {
		t.Fatalf("steer inbox after a default-sender steer = %+v", posts)
	}

	// A weave worker with no socket keeps the line for its resume.
	if err := room.Join(room.Card{ID: "weave-1", Mode: "weave", PID: os.Getpid(), Caps: []string{SteerInboxPrefix + "steer-w"}}); err != nil {
		t.Fatal(err)
	}
	if rec, err := Steer("weave-1", "alice", "later"); err != nil || !rec.Queued || !rec.Headless {
		t.Fatalf("headless worker: %+v, %v", rec, err)
	}
}

// A session with only a control socket is typed into directly, and the steer
// is recorded on the room timeline under the sender's name.
func TestSteerTypesIntoASessionWithOnlyASocket(t *testing.T) {
	frames := steerAPIEnv(t)
	if err := room.Join(room.Card{ID: "claude", Mode: "interactive", PID: os.Getpid(), CtlSock: "ctl-claude"}); err != nil {
		t.Fatal(err)
	}
	rec, err := Steer("claude", "alice", "look at tab 7")
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if rec.ID != "claude" || rec.Queued || rec.Headless {
		t.Fatalf("receipt = %+v, want a direct steer", rec)
	}
	if want := "ctl-claude|" + agentpty.TextFrame("look at tab 7"); len(*frames) != 1 || (*frames)[0] != want {
		t.Fatalf("frames = %q, want %q", *frames, want)
	}
	if ev := steerEvents(t, "claude"); len(ev) != 1 || ev[0].Actor != "alice" || ev[0].Body != "look at tab 7" {
		t.Fatalf("steer events = %+v", ev)
	}
}

// Nothing is sent to a session that is not live, or with nothing to say.
func TestSteerRefusesAnUnknownSessionAndEmptyText(t *testing.T) {
	frames := steerAPIEnv(t)
	if err := room.Join(room.Card{ID: "claude", Mode: "interactive", PID: os.Getpid(), CtlSock: "ctl-claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Steer("nobody-here", "alice", "hello"); err == nil || !strings.Contains(err.Error(), "nothing live matches") {
		t.Fatalf("unknown session: err = %v", err)
	}
	if _, err := Steer("claude", "alice", " \n"); err == nil {
		t.Fatal("an empty steer was accepted")
	}
	if len(*frames) != 0 || len(steerEvents(t, "claude")) != 0 {
		t.Fatalf("a refused steer reached the session: %q", *frames)
	}
}

func TestSteerableCard(t *testing.T) {
	for name, c := range map[string]struct {
		card room.Card
		want bool
	}{
		"steer inbox":      {room.Card{Caps: []string{SteerInboxPrefix + "x"}}, true},
		"control socket":   {room.Card{CtlSock: "/tmp/x.sock"}, true},
		"mail only":        {room.Card{Caps: []string{room.CapInboxDelivery}}, false},
		"blank socket":     {room.Card{CtlSock: "  "}, false},
		"nothing declared": {room.Card{}, false},
	} {
		if got := SteerableCard(c.card); got != c.want {
			t.Errorf("%s: SteerableCard = %v, want %v", name, got, c.want)
		}
	}
}
