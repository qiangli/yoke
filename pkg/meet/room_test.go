package meet

import (
	"testing"
)

func saveMeeting(t *testing.T, id string, room int, status string) *State {
	t.Helper()
	st := &State{ID: id, Room: room, Status: status, Topic: id, Secretary: "claude"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	return st
}

// Rooms are the lowest free number among the OPEN meetings — shell job numbers,
// not an ever-growing ledger. Nobody wants to attach to room 47.
func TestAssignRoomTakesTheLowestFreeNumber(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())

	if got := assignRoom(); got != 1 {
		t.Fatalf("the first meeting is room 1, got %d", got)
	}
	saveMeeting(t, "a", 1, "open")
	if got := assignRoom(); got != 2 {
		t.Fatalf("with room 1 occupied the next is 2, got %d", got)
	}
	saveMeeting(t, "b", 2, "open")
	saveMeeting(t, "c", 3, "open")
	if got := assignRoom(); got != 4 {
		t.Fatalf("want 4, got %d", got)
	}

	// A closed meeting RELEASES its room. Rooms are a small set of doors.
	saveMeeting(t, "b", 2, "closed")
	if got := assignRoom(); got != 2 {
		t.Fatalf("a closed meeting's room must be reused, got %d", got)
	}
}

// The room is stored on the meeting, not derived from its position in a list.
// Position would re-point under you: open a new meeting and yesterday's room 2
// silently becomes room 3, so a number you read a minute ago now attaches you
// to a different discussion.
func TestRoomDoesNotShiftWhenAnotherMeetingOpens(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())

	saveMeeting(t, "first", 1, "open")
	saveMeeting(t, "second", 2, "open")

	// A third meeting opens. The first two keep their doors.
	saveMeeting(t, "third", assignRoom(), "open")

	for room, want := range map[string]string{"1": "first", "2": "second", "3": "third"} {
		got, err := resolveMeeting(room)
		if err != nil {
			t.Fatalf("room %s: %v", room, err)
		}
		if got != want {
			t.Errorf("room %s resolved to %q, want %q — a room must not move under the user", room, got, want)
		}
	}
}

// Attaching to "room 2" plainly means whoever is sitting in it NOW. A closed
// meeting has released its room, so an open one holding that number wins.
func TestResolveRoomPrefersTheOpenMeeting(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	saveMeeting(t, "old", 2, "closed")
	saveMeeting(t, "now", 2, "open")

	got, err := resolveMeeting("2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "now" {
		t.Errorf("room 2 = %q, want the meeting currently in it", got)
	}
}

// The id stays the identity; the room is only a way of typing it. Both work,
// and so does an unambiguous prefix — forty characters is not a thing to retype.
func TestResolveAcceptsIDAndPrefix(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	saveMeeting(t, "2026-07-13-cache-write-through-0e1e", 1, "open")
	saveMeeting(t, "2026-07-13-band-schema-9f2a", 2, "open")

	for _, ref := range []string{
		"2026-07-13-cache-write-through-0e1e", // the full id
		"2026-07-13-cache",                    // an unambiguous prefix
		"1",                                   // its room
	} {
		got, err := resolveMeeting(ref)
		if err != nil {
			t.Fatalf("%q: %v", ref, err)
		}
		if got != "2026-07-13-cache-write-through-0e1e" {
			t.Errorf("%q resolved to %q", ref, got)
		}
	}
}

// An ambiguous prefix is an error, never a guess. Silently attaching to the
// wrong meeting is worse than being asked to be specific: you would sit and
// watch a discussion believing it was a different one.
func TestResolveRefusesAnAmbiguousPrefix(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	saveMeeting(t, "2026-07-13-cache-aaaa", 1, "open")
	saveMeeting(t, "2026-07-13-cache-bbbb", 2, "open")

	if _, err := resolveMeeting("2026-07-13-cache"); err == nil {
		t.Fatal("an ambiguous prefix must be refused, not guessed at")
	}
}

func TestResolveEmptyRoomSaysSo(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	saveMeeting(t, "only", 1, "open")

	if _, err := resolveMeeting("7"); err == nil {
		t.Fatal("attaching to an empty room must fail")
	}
}

// A meeting from before rooms existed shows a dash, not a fake room 0 that
// somebody would then try to attach to.
func TestRoomLabelForLegacyMeeting(t *testing.T) {
	if got := roomLabel(&State{}); got != "-" {
		t.Errorf("an unassigned room renders as %q, want -", got)
	}
	if got := roomLabel(&State{Room: 3}); got != "3" {
		t.Errorf("roomLabel = %q, want 3", got)
	}
}

// A meeting that predates rooms has no door, and a meeting you cannot attach to
// is the one thing rooms exist to fix. It gets one on first sight — and the
// door is PERSISTED, because a number that changed between the `list` that
// showed it and the `observe` that used it would be worse than no number.
func TestOpenRoomsBackfillsAndPersists(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	saveMeeting(t, "legacy-open", 0, "open")
	saveMeeting(t, "legacy-closed", 0, "closed")

	sessions, err := openRooms()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*State{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	if byID["legacy-open"].Room != 1 {
		t.Errorf("an open meeting must get a door, got room %d", byID["legacy-open"].Room)
	}
	// A closed meeting has nothing to watch; handing it a number burns a door.
	if byID["legacy-closed"].Room != 0 {
		t.Errorf("a closed meeting needs no door, got room %d", byID["legacy-closed"].Room)
	}

	// Persisted: the same door on the next call, and on the next process.
	reloaded, err := loadState("legacy-open")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Room != 1 {
		t.Errorf("the backfilled room must be saved, got %d", reloaded.Room)
	}
	if got, err := resolveMeeting("1"); err != nil || got != "legacy-open" {
		t.Errorf("resolveMeeting(1) = %q, %v", got, err)
	}
}
