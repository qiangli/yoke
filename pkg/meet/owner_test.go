package meet

import (
	"encoding/json"
	"testing"
)

// A sprint's room is owned by its PROJECT MANAGER, and the browser must be
// able to say who that is right now.
//
// The room stores a late-bound label ("conductor:99") precisely so the holder
// is decided at READ time. A browser cannot resolve one — the seat table lives
// on the host — so before this the SPA had a room with an accountable owner and
// no way to name them: it offered a roster in which the person answerable for
// the sprint looked like any other agent.
func TestRoomViewNamesTheSprintsProjectManager(t *testing.T) {
	st := newRoom(t)
	st.DefaultTo = "conductor:99"
	st.Chair = "some-facilitator"
	withSeat(t, "conductor:99", "conductor.99", "trestle")

	v := decodeView(t, viewOf(st))
	if v.Owner != "trestle" {
		t.Errorf("owner = %q, want the seat's current holder", v.Owner)
	}
	if v.OwnerTitle != "project manager" {
		t.Errorf("owner_title = %q, want the sprint domain's word for its owner", v.OwnerTitle)
	}
}

// The seat OUTRANKS the facilitator. Unaddressed mail in this room already
// lands on the seat, so showing the facilitator instead would point a reader at
// someone who is not the one answering.
func TestRoomViewPrefersTheSeatOverTheFacilitator(t *testing.T) {
	st := newRoom(t)
	st.DefaultTo = "conductor:99"
	st.Chair = "some-facilitator"
	withSeat(t, "conductor:99", "conductor.99", "trestle")

	if v := decodeView(t, viewOf(st)); v.Owner == "some-facilitator" {
		t.Fatal("the facilitator was shown as owner of a room whose mail goes to a seat")
	}
}

// A VACANT seat still has a title. Falling back to the facilitator here would
// answer a different question than the one asked — the room's project-manager
// seat is empty, and "empty" is the true answer.
func TestRoomViewReportsAVacantSeatAsVacant(t *testing.T) {
	st := newRoom(t)
	st.DefaultTo = "conductor:99"
	st.Chair = "some-facilitator"
	withSeat(t, "conductor:99", "conductor.99", "")

	v := decodeView(t, viewOf(st))
	if v.Owner != "" {
		t.Errorf("owner = %q, want nobody — the seat has no holder", v.Owner)
	}
	if v.OwnerTitle != "project manager" {
		t.Errorf("owner_title = %q, want the seat's title even while it is vacant", v.OwnerTitle)
	}
}

// An ordinary meeting's owner is its FACILITATOR — the seat `--owner` sets.
func TestRoomViewNamesAnOrdinaryMeetingsFacilitator(t *testing.T) {
	st := newRoom(t)
	st.Chair = "codex"

	v := decodeView(t, viewOf(st))
	if v.Owner != "codex" || v.OwnerTitle != "facilitator" {
		t.Fatalf("owner = %q/%q, want codex/facilitator", v.Owner, v.OwnerTitle)
	}
}

// The projection must not become a STORED field. State is persisted with the
// same marshaller, and a copied holder is the exact bug the late-bound label
// exists to prevent: a handover would leave the saved name naming somebody who
// no longer answers.
func TestRoomStateDoesNotPersistAResolvedOwner(t *testing.T) {
	st := newRoom(t)
	st.DefaultTo = "conductor:99"
	withSeat(t, "conductor:99", "conductor.99", "trestle")
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := roomOf(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(reloaded)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["owner"]; ok {
		t.Error("a resolved owner was written onto the stored state; it must exist only in the view")
	}
}

func decodeView(t *testing.T, v any) roomView {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out roomView
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
