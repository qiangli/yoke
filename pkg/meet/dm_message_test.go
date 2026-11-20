package meet

import (
	"os"
	"strings"
	"testing"
)

// A direct message from outside the Meet app is ordinary mail: one human event
// in the durable DM room, addressed to the agent, with the text exactly as it
// was given. It is the agent's unread directed work and nobody else's, and
// recording it runs no agent.
func TestDirectMessageIsDirectedMailInTheDMRoom(t *testing.T) {
	setupDMFrontDoorEnv(t)
	calls := stubInvoke(t, "should never run")

	// Longer than a board post may be, with line breaks and markup: the DM
	// room is the long-body path and keeps all of it.
	text := "Shape from tester's browser\n\n  <b>indented</b> & \"quoted\"\n" + strings.Repeat("long line ", 300) + "\nend\n"
	rec, err := DirectMessage(" @codex ", "", text)
	if err != nil {
		t.Fatalf("DirectMessage: %v", err)
	}
	if rec.Agent != "codex" || rec.Human != "tester" {
		t.Fatalf("receipt = %+v, want codex addressed by the login user", rec)
	}
	st, err := dmRoomFor("codex", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Room != st.ID {
		t.Fatalf("receipt names room %q, the DM room is %q", rec.Room, st.ID)
	}
	if len(*calls) != 0 {
		t.Fatalf("recording a message ran an agent turn: %+v", *calls)
	}

	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for _, e := range events {
		if e.Kind == "human" {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("human events = %d, want 1: %+v", len(got), events)
	}
	if e := got[0]; e.Speaker != "tester" || e.To != "codex" || e.Text != text {
		t.Fatalf("event = speaker %q to %q, text verbatim %v", e.Speaker, e.To, e.Text == text)
	}

	directed, _, _, err := Unread(st.ID, "codex", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 1 || directed[0].Text != text {
		t.Fatalf("codex's unread directed mail = %d events, want this one", len(directed))
	}
	if mine, _, _, err := Unread(st.ID, "tester", 10); err != nil || len(mine) != 0 {
		t.Fatalf("the sender got their own message back as mail: %d events, err %v", len(mine), err)
	}

	// A second message joins the same room rather than opening another.
	if again, err := DirectMessage("codex", "tester", "and one more"); err != nil || again.Room != rec.Room {
		t.Fatalf("second message: room %q err %v, want room %q", again.Room, err, rec.Room)
	}
}

// Mail needs somebody to be addressed to and something to say. A name the
// fleet does not know is refused before any room is created for it.
func TestDirectMessageRefusesWhatCannotBeMail(t *testing.T) {
	setupDMFrontDoorEnv(t)
	for name, c := range map[string]struct{ agent, text string }{
		"no agent":           {"  ", "hello"},
		"empty text":         {"codex", " \n\t"},
		"unregistered agent": {"no-such-agent-anywhere", "hello"},
	} {
		if rec, err := DirectMessage(c.agent, "tester", c.text); err == nil {
			t.Errorf("%s: recorded as %+v, want an error", name, rec)
		}
	}
	left, err := os.ReadDir(os.Getenv("BASHY_MEET_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("a refused message left %d entries in the meet store", len(left))
	}
}
