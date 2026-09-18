package meet

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A meeting id is a space-time coordinate: unique forever, and far too long to
// type. `2026-07-13-should-the-cache-be-write-through-0e1e` is the right thing
// to STORE and the wrong thing to ask a human to retype in order to look in on
// a discussion.
//
// So a meeting also gets a ROOM: a small number you can actually say. Room 2.
// Same idea as a shell's job numbers — `%1`, `%2` — and deliberately the same
// semantics, because you already know them:
//
//   - A room is assigned from the LOWEST FREE number among the meetings that
//     are still open. Three meetings running means rooms 1, 2, 3.
//   - It is REUSED once the meeting closes. Rooms are a small set of doors, not
//     an ever-growing ledger; nobody wants to attach to room 47.
//   - It is a POINTER, never an identity. It is resolved the moment you type it
//     and is never written into a record — a transcript that said "room 2
//     decided X" would rot the instant room 2 was reused, which is the exact
//     failure the version-explicit model names exist to prevent.
//
// The number is stored on the meeting rather than derived from its position in
// a list. Position would re-point under you: open a new meeting and yesterday's
// room 2 silently becomes room 3, so the number you read a minute ago now
// attaches you to a different discussion. A room is assigned once, held for the
// life of the meeting, and released at the end.

// assignRoom returns the lowest room number not held by an open meeting.
func assignRoom() int {
	sessions, err := listSessions()
	if err != nil {
		return 1
	}
	return lowestFreeRoom(sessions)
}

func lowestFreeRoom(sessions []*State) int {
	taken := map[int]bool{}
	for _, s := range sessions {
		if s.Status == "open" && s.Room > 0 {
			taken[s.Room] = true
		}
	}
	for n := 1; ; n++ {
		if !taken[n] {
			return n
		}
	}
}

// openRooms returns every meeting, giving a room to any OPEN one that has none.
//
// Meetings that predate rooms have no door, and a meeting you cannot attach to
// is the one thing rooms exist to fix — so they get one on first sight, and it
// is persisted, because a door that renumbered itself between the `list` that
// showed it and the `observe` that used it would be worse than no door at all.
//
// Only open meetings are backfilled. A closed meeting has nothing to watch, and
// handing it a number would use up a door for no reason.
func openRooms() ([]*State, error) {
	sessions, err := listSessions()
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.Status != "open" || s.Room > 0 {
			continue
		}
		s.Room = lowestFreeRoom(sessions)
		_ = s.save() // best-effort: an unwritable store must not break `list`
	}
	return sessions, nil
}

// resolveMeeting turns whatever a human typed into a meeting id.
//
// It accepts, in order: a room number ("2"), a permanent name ("steward" or
// "@steward"), a full id, or an unambiguous PREFIX of an id — because even
// when you do reach for the id, you should not have to type all forty chars.
//
// An ambiguous prefix is an error, never a guess. Attaching to the wrong
// meeting is worse than being told to be more specific: you would sit and watch
// a discussion believing it was a different one.
func resolveMeeting(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("meet: no meeting named")
	}
	sessions, err := openRooms()
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		return "", fmt.Errorf("meet: no meetings on this host")
	}

	// A room number. Open meetings win: a closed meeting's room has been
	// released, and "attach to room 2" plainly means the one sitting in it now.
	if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil {
		var closed *State
		for _, s := range sessions { // newest-first
			if s.Room != n {
				continue
			}
			if s.Status == "open" {
				return s.ID, nil
			}
			if closed == nil {
				closed = s
			}
		}
		if closed != nil {
			return closed.ID, nil
		}
		return "", fmt.Errorf("meet: nobody is in room %d — `bashy meet list`", n)
	}

	name := strings.ToLower(strings.TrimPrefix(ref, "@"))
	var named *State
	for _, s := range sessions {
		if !s.Permanent || s.Name != name {
			continue
		}
		if named != nil {
			return "", fmt.Errorf("meet: permanent room %q has duplicate identities", name)
		}
		named = s
	}
	if named != nil {
		return named.ID, nil
	}

	for _, s := range sessions {
		if s.ID == ref {
			return s.ID, nil
		}
	}

	var hits []string
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, ref) {
			hits = append(hits, s.ID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("meet: no meeting matches %q — `bashy meet list`", ref)
	default:
		return "", fmt.Errorf("meet: %q matches %d meetings (%s) — be more specific, or use its room number",
			ref, len(hits), strings.Join(hits, ", "))
	}
}

// directMessageRoomName returns the one stable permanent-room address for two
// seats. Canonicalization happens before sorting, so aliases and reversed
// callers converge; slugify supplies the existing portable room-name rules.
// It deliberately does not call newID: that function is unique-per-instance,
// while a DM is one durable address reused forever.
func directMessageRoomName(a, b string) (string, error) {
	seats := []string{slugify(canonAgent(a)), slugify(canonAgent(b))}
	sort.Strings(seats)
	return permanentRoomName(slugify("dm-" + strings.Join(seats, "-")))
}

// loadMeeting resolves whatever a caller typed — a room number, an id, or an
// unambiguous prefix — and loads that session.
//
// It is the front door for anything that takes a room REFERENCE rather than an
// id: a human typing `2`, and an HTTP layer receiving `:ref` from a URL, are the
// same lookup, and duplicating it would let the two drift apart on which
// spellings are accepted.
func loadMeeting(ref string) (*State, error) {
	id, err := resolveMeeting(ref)
	if err != nil {
		return nil, err
	}
	return loadState(id)
}

// roomLabel renders a room for the list. A meeting from before rooms existed,
// or one whose room was never assigned, shows a dash rather than a fake 0.
func roomLabel(s *State) string {
	if s.Room < 1 {
		return "-"
	}
	return strconv.Itoa(s.Room)
}
