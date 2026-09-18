package meet

import (
	"strings"
	"testing"
)

// A steer only reaches an agent that is mid-turn. The live channel is what says
// who that is: a `speaking` with no matching `spoke`.
func TestCurrentSpeakerTracksTheFloor(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	if _, err := currentSpeaker(st.ID); err == nil {
		t.Fatal("with no turn in flight there is nobody to steer")
	}

	w := newLiveWriter(st, "claude-fable5", "", "/tmp/x.sock")
	floor, err := currentSpeaker(st.ID)
	if err != nil {
		t.Fatalf("an agent mid-turn must be steerable: %v", err)
	}
	if floor.Speaker != "claude-fable5" || floor.CtlSock != "/tmp/x.sock" {
		t.Fatalf("floor = %+v", floor)
	}

	// It finishes. A line sent now would be typed into a closed room.
	w.close(statusOK)
	if _, err := currentSpeaker(st.ID); err == nil {
		t.Fatal("a finished turn must not be steerable")
	}
}

// The floor passes from one agent to the next, and a steer must follow it.
// Addressing the previous speaker is the one way a steer goes quietly nowhere.
func TestCurrentSpeakerFollowsTheFloor(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	newLiveWriter(st, "claude-fable5", "", "/tmp/a.sock").close(statusOK)
	st.Round = 2
	newLiveWriter(st, "codex-gpt-5.5", "", "/tmp/b.sock")

	floor, err := currentSpeaker(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if floor.Speaker != "codex-gpt-5.5" || floor.CtlSock != "/tmp/b.sock" || floor.Round != 2 {
		t.Fatalf("the floor must follow the current speaker, got %+v", floor)
	}
}

// Unix caps a socket address at ~104 bytes. A meeting id is a slug of its topic,
// so putting the socket under the meeting's own directory would blow the cap and
// silently degrade to a polling file channel.
func TestCtlSockPathStaysWithinTheUnixLimit(t *testing.T) {
	st := &State{
		ID:    "2026-07-13-" + strings.Repeat("a-very-long-topic-", 12) + "0e1e",
		Round: 3,
	}
	path := ctlSockPath(st, "opencode-kimi-k2.7-code")
	if path == "" || len(path) > 100 {
		t.Fatalf("socket path is %d bytes, past the ~104 unix limit: %s", len(path), path)
	}
	if again := ctlSockPath(st, "opencode-kimi-k2.7-code"); again != path {
		t.Error("the same turn must keep the same socket")
	}
	// Two seats, or two rounds, must never share a control channel — a steer
	// aimed at one would land in the other.
	if other := ctlSockPath(st, "claude-fable5"); other == path {
		t.Error("two seats must not share a control channel")
	}
	st.Round = 4
	if next := ctlSockPath(st, "opencode-kimi-k2.7-code"); next == path {
		t.Error("two rounds must not share a control channel")
	}
}
