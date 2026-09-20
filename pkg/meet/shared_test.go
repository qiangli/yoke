package meet

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// newSharedBoard is a board this host shares over a session, with one local
// agent seated and the test's human convening it.
func newSharedBoard(t *testing.T) *State {
	t.Helper()
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	fleettest.Ring(t)
	t.Setenv("USER", "qiangli")
	old := nowFn
	nowFn = fixedNow
	t.Cleanup(func() { nowFn = old })
	prevA, prevP := registeredAgentFn, registeredPersonFn
	registeredAgentFn = func(name string) (fleet.Agent, bool) {
		if name == "codex" || name == "rafter" {
			return fleet.Agent{Name: name}, true
		}
		return fleet.Agent{}, false
	}
	registeredPersonFn = func(name string) bool { return name == "bob" || name == "carol" }
	t.Cleanup(func() { registeredAgentFn, registeredPersonFn = prevA, prevP })
	st := &State{
		ID: newID("Shared room", fixedNow()), Topic: "Shared room",
		Participants: []string{"codex"}, Human: "qiangli", Initiator: "qiangli",
		Board: true, Shared: true, Session: "task-1",
		Status: "open", Cwd: t.TempDir(), Created: fixedNow(),
	}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	return st
}

// People are seats: a registered person is invited, posts, is addressed,
// and is kicked — never scheduled (Observers, not Participants).
func TestPeopleAreSeatsOnABoard(t *testing.T) {
	st := newSharedBoard(t)
	if err := Invite(st.ID, "qiangli", "bob"); err != nil {
		t.Fatalf("invite a person: %v", err)
	}
	if err := Invite(st.ID, "qiangli", "bob"); err != nil {
		t.Fatalf("invite twice must be a no-op: %v", err)
	}
	st, _ = loadState(st.ID)
	if len(st.Observers) != 1 || st.Observers[0] != "bob" || len(st.Participants) != 1 {
		t.Fatalf("roster after invite: observers=%v participants=%v", st.Observers, st.Participants)
	}
	if err := Invite(st.ID, "qiangli", "nobody"); err == nil || !strings.Contains(err.Error(), "not a registered agent") {
		t.Fatalf("an unknown name is still refused: %v", err)
	}
	// bob posts, and is addressed — as a seated attendee.
	if !participantSeat(st, "bob") {
		t.Fatal("a seated person must count as a seat for posting")
	}
	if err := Kick(st.ID, "qiangli", "bob"); err != nil {
		t.Fatalf("kick a person: %v", err)
	}
	st, _ = loadState(st.ID)
	if len(st.Observers) != 0 {
		t.Fatalf("observers after kick: %v", st.Observers)
	}
}

// A room with no agent at all — people only — opens and seats people.
func TestHumansOnlyRoom(t *testing.T) {
	st := newSharedBoard(t)
	st.Participants = nil
	if err := st.Validate(); err != nil {
		t.Fatalf("a humans-only board must be valid: %v", err)
	}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if err := Invite(st.ID, "qiangli", "carol"); err != nil {
		t.Fatal(err)
	}
	st, _ = loadState(st.ID)
	if !participantSeat(st, "carol") || !participantSeat(st, "qiangli") {
		t.Fatal("both people must be able to post")
	}
}

// A post from the feed is filed into a mirror created on first sight,
// exactly once per message id, seating what it names as THIS host knows
// them: <name>@<myhost> is the local name; other seats are attendees.
func TestDeliverSharedCreatesMirrorAndIsIdempotent(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	fleettest.Ring(t)
	t.Setenv("USER", "noviadmin")
	prevA := registeredAgentFn
	registeredAgentFn = func(name string) (fleet.Agent, bool) {
		if name == "rafter" {
			return fleet.Agent{Name: "rafter"}, true
		}
		return fleet.Agent{}, false
	}
	t.Cleanup(func() { registeredAgentFn = prevA })

	ev := SharedEvent{
		ID: "0192f3a4-7c1e-7000-8000-000000000009", RoomID: "2026-09-19-shared-room-abcd",
		Topic: "Shared room", Session: "task-1",
		Roster: []string{"codex@hostA", "qiangli@hostA", "bob@hostA", "rafter@hostB"},
		From:   "codex@hostA", To: "rafter@hostB", Body: "rebase first", At: fixedNow(),
	}
	filed, err := DeliverShared(ev, "hostB")
	if err != nil || !filed {
		t.Fatalf("first delivery: filed=%v err=%v", filed, err)
	}
	st, err := loadState(ev.RoomID)
	if err != nil {
		t.Fatalf("mirror not created: %v", err)
	}
	if !st.Shared || !st.Board || st.Session != "task-1" || st.Topic != "Shared room" {
		t.Fatalf("mirror = %+v", st)
	}
	if len(st.Participants) != 1 || st.Participants[0] != "rafter" {
		t.Fatalf("the local agent must sit as a participant: %v", st.Participants)
	}
	for _, want := range []string{"codex@hostA", "qiangli@hostA", "bob@hostA"} {
		if !st.humanAttendee(want) {
			t.Fatalf("remote seat %s must be an attendee; observers=%v", want, st.Observers)
		}
	}
	events, _ := readTranscript(st.ID)
	if len(events) != 1 || events[0].Speaker != "codex@hostA" || events[0].To != "rafter" || events[0].Text != "rebase first" || events[0].Origin == nil || events[0].Origin.Source != "session:"+ev.ID {
		t.Fatalf("transcript = %+v", events)
	}
	// Redelivery is a no-op.
	filed, err = DeliverShared(ev, "hostB")
	if err != nil || filed {
		t.Fatalf("redelivery: filed=%v err=%v", filed, err)
	}
	if events, _ = readTranscript(st.ID); len(events) != 1 {
		t.Fatalf("duplicate filed: %d events", len(events))
	}
	// A local, never-shared room by the same id is never merged into.
	local := &State{ID: "local-only", Topic: "mine", Board: true, Human: "noviadmin", Initiator: "noviadmin", Status: "open", Created: fixedNow()}
	_ = local.save()
	if _, err := DeliverShared(SharedEvent{ID: "x", RoomID: "local-only", From: "a@b", Body: "hi"}, "hostB"); err == nil {
		t.Fatal("a feed must not be merged into a room that is not shared")
	}
}

// A shared board post carries a universal id and reaches the relay seam.
func TestSharedBoardPostRelays(t *testing.T) {
	st := newSharedBoard(t)
	var relayed []Event
	prev := RelayShared
	RelayShared = func(s *State, ev Event) error { relayed = append(relayed, ev); return nil }
	t.Cleanup(func() { RelayShared = prev })
	ev, err := PostAs(st.ID, "codex", "", "hello everyone")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Origin == nil || !strings.HasPrefix(ev.Origin.Source, "session:") || len(relayed) != 1 || relayed[0].Text != "hello everyone" {
		t.Fatalf("ev=%+v relayed=%d", ev, len(relayed))
	}
}
