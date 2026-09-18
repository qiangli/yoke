package meet

import (
	"context"
	"testing"
)

// A message meant for the whole room must land in EVERY participant's
// actionable inbox.
//
// Before this it could not be said at all from the browser, and saying nothing
// was not the same thing: an unaddressed post is room HISTORY — everyone can
// read it, nobody owes a reply, and dispatch wakes no one. So "message the
// room" reached the transcript and no inbox, which reads from the outside
// exactly like every agent ignoring it.
func TestBroadcastReachesEveryParticipantsInbox(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"alpha", "beta"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	ev, err := PostAs(st.ID, "qiangli", AllSeats, "standup in five")
	if err != nil {
		t.Fatal(err)
	}
	if ev.To != AllSeats {
		t.Fatalf("To = %q, want the broadcast addressee recorded verbatim", ev.To)
	}
	for _, who := range []string{"alpha", "beta"} {
		directed, _, _, _, err := UnreadRecords(st.ID, who, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(directed) != 1 {
			t.Errorf("%s has %d directed record(s), want the broadcast", who, len(directed))
		}
	}
}

// An UNADDRESSED post stays room history. This is the invariant the broadcast
// is built beside rather than on top of: if saying nothing meant "everybody",
// each of N replies — which are unaddressed — would wake the other N-1 and the
// room would never settle.
func TestAnUnaddressedPostIsStillRoomHistory(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"alpha", "beta"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := Post(st.ID, "qiangli", "thinking out loud"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"alpha", "beta"} {
		directed, other, _, _, err := UnreadRecords(st.ID, who, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(directed) != 0 {
			t.Errorf("%s was handed %d unaddressed record(s) as work", who, len(directed))
		}
		if len(other) != 1 {
			t.Errorf("%s cannot see the room's own history (%d records)", who, len(other))
		}
	}
}

// A broadcast must NOT be redirected to the room's default seat.
//
// THE FAILURE THIS ENCODES: in a sprint's room, an empty addressee falls
// through to DefaultTo (`conductor:99`). A UI offering "Everyone" on top of an
// empty addressee therefore sent to the conductor ALONE — and because directed
// mail is filtered out of every other reader's history bucket, the other
// participants saw NOTHING AT ALL. The author was told it went to the room.
func TestBroadcastIsNotRedirectedToTheSprintSeat(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"alpha", "beta"}
	st.DefaultTo = "conductor:99"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	withSeat(t, "conductor:99", "conductor.99", "alpha")

	ev, err := PostAs(st.ID, "qiangli", AllSeats, "everyone please read")
	if err != nil {
		t.Fatal(err)
	}
	if ev.To != AllSeats {
		t.Fatalf("a broadcast was rewritten to %q — the room's default seat swallowed it", ev.To)
	}
	for _, who := range []string{"alpha", "beta"} {
		directed, _, _, _, err := UnreadRecords(st.ID, who, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(directed) != 1 {
			t.Errorf("%s has %d directed record(s); a broadcast in a sprint room "+
				"must reach every seat, not only the conductor's", who, len(directed))
		}
	}
}

// A broadcast wakes each participant ONCE, and the turns it provokes wake
// nobody — that bound is what makes it safe to have at all.
func TestBroadcastWakesEachParticipantOnceAndTerminates(t *testing.T) {
	st := newRoom(t)
	st.Participants = []string{"alpha", "beta"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	withFakeAgent(t, "ack")

	if _, err := PostAs(st.ID, "qiangli", AllSeats, "status?"); err != nil {
		t.Fatal(err)
	}
	first, err := Dispatch(context.Background(), st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("dispatch woke %d participant(s), want both", len(first))
	}
	for _, d := range first {
		if d.Err != nil {
			t.Fatalf("%s: %v", d.Agent, d.Err)
		}
		if d.Reply.To != "" {
			t.Errorf("%s replied ADDRESSED to %q — a reply that carries an addressee "+
				"would wake the others, and the cascade never ends", d.Agent, d.Reply.To)
		}
	}
	// The replies are in the transcript now. A second pass must find nothing.
	second, err := Dispatch(context.Background(), st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("the broadcast's own replies woke %d more turn(s): %+v", len(second), second)
	}
}

// The addressee a person types is not the one the code compares.
func TestBroadcastAcceptsWhatAPersonTypes(t *testing.T) {
	for _, typed := range []string{"all", "All", "@all", " all "} {
		if !isAllSeats(typed) {
			t.Errorf("%q is not recognised as the broadcast addressee", typed)
		}
	}
	for _, typed := range []string{"", "alpha", "allen", "all-hands"} {
		if isAllSeats(typed) {
			t.Errorf("%q was mistaken for the broadcast addressee", typed)
		}
	}
}
