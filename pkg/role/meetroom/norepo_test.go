package meetroom

import (
	"errors"
	"testing"

	"github.com/qiangli/yoke/pkg/meet"
	"github.com/qiangli/yoke/pkg/role"
)

// A ROLE ROOM MUST NEVER PUBLISH MINUTES INTO THE REPO.
//
// meet's default sends minutes to <repo>/docs/meetings/, which suits a meeting a
// person convened. A role room is not that: pkg/role opens one on every assume,
// so the default left a pair of empty minutes in the source tree per lease
// acquire — 28 files in one afternoon in coreutils.
//
// The churn is not why this is pinned. Minutes name their attendees, so each
// file carried a real hostname and a real OS user, and coreutils ships as public
// MIT source. Nothing in the flow prompts anyone to read a generated file before
// `git add`, so the leak is silent by construction — which is exactly the kind
// that needs a test rather than a convention.
func TestAssume_MinutesNeverLandInTheRepo(t *testing.T) {
	if meet.OutStore == "" {
		t.Fatal("OutStore sentinel is empty — minutes would fall back to the repo")
	}
	for _, k := range []role.Kind{role.Steward, role.Conductor} {
		a := role.Assignment{Kind: k, Ref: "r1", Title: "t"}
		c, err := Assume(a, "agent-1")
		if err != nil {
			// Assume needs a usable meet store; skip rather than fail where it
			// cannot run, but never let that hide the sentinel check above.
			t.Skipf("meet unavailable for %v: %v", k, err)
		}
		if c == nil || c.Ref == "" {
			t.Fatalf("%v: no contact returned", k)
		}
		st, _, rerr := meet.Room(c.Ref)
		if rerr != nil || st == nil {
			t.Skipf("cannot read back room: %v", rerr)
		}
		if st.Out != meet.OutStore {
			t.Errorf("%v: Out = %q, want %q — minutes would land in the repo",
				k, st.Out, meet.OutStore)
		}
		_ = Release(c, "agent-1")
	}
}

// A ROLE ROOM DECLARES ITS SEAT AS THE DEFAULT ADDRESSEE.
//
// The room is the place a sprint advertises for questions, so an unaddressed
// message there is not addressed to nobody — it belongs to whoever is
// accountable. The label is stored, never the holder: the room outlives its
// conductor by design, and mail that named the person holding the lease when it
// was written would go with them at exactly the handoff the room exists for.
func TestRoleRoomDeclaresItsSeatAsDefaultAddressee(t *testing.T) {
	var got meet.CreateOptions
	prev := createRoom
	createRoom = func(opts meet.CreateOptions) (*meet.State, error) {
		got = opts
		return &meet.State{ID: "room-1", Room: 1}, nil
	}
	t.Cleanup(func() { createRoom = prev })

	if _, err := Assume(role.Assignment{Kind: role.Conductor, Ref: "99"}, "trestle"); err != nil {
		t.Fatal(err)
	}
	if got.DefaultTo != "conductor:99" {
		t.Fatalf("DefaultTo = %q, want the seat label a person types", got.DefaultTo)
	}
	if got.DefaultTo == "trestle" {
		t.Fatal("the room stored its holder — mail would not survive a handover")
	}
	if got.Name != "sprint 99" {
		t.Fatalf("Name = %q, want the sprint's stable room name", got.Name)
	}
}

func TestSprintRoomNameDoesNotDependOnManagerOrTitle(t *testing.T) {
	first := role.Assignment{Kind: role.Conductor, Ref: "123", Title: "initial title"}
	second := role.Assignment{Kind: role.Conductor, Ref: "123", Title: "renamed later"}
	if got := roomNameFor(first); got != "sprint 123" {
		t.Fatalf("roomNameFor(first) = %q", got)
	}
	if got := roomNameFor(second); got != "sprint 123" {
		t.Fatalf("roomNameFor(second) = %q; a title edit changed room identity", got)
	}
}

// A ROOM THAT PREDATES THE FIELD MUST STILL GET ONE.
//
// DefaultTo was added to a codebase that already had rooms. Set only at Create,
// it is inert on every existing room — on this host that was all of them,
// including the single sprint room the feature was written for, so the shipped
// behavior was observably a no-op where it mattered most.
func TestEnsureDefaultToDeclaresTheSeatOnAnExistingRoom(t *testing.T) {
	var gotRef, gotLabel string
	prev := setRoomDefaultTo
	setRoomDefaultTo = func(ref, label string) error {
		gotRef, gotLabel = ref, label
		return nil
	}
	t.Cleanup(func() { setRoomDefaultTo = prev })

	c := &role.Contact{Kind: "meet", Ref: "room-1"}
	if err := EnsureDefaultTo(c, role.Assignment{Kind: role.Conductor, Ref: "99"}); err != nil {
		t.Fatal(err)
	}
	if gotRef != "room-1" || gotLabel != "conductor:99" {
		t.Fatalf("EnsureDefaultTo set (%q, %q), want the room and its seat label", gotRef, gotLabel)
	}
}

// No contact is not an error. A sprint with no room is an ordinary state, and
// healing must never be the thing that reports it.
func TestEnsureDefaultToIsAQuietNoOpWithoutARoom(t *testing.T) {
	prev := setRoomDefaultTo
	setRoomDefaultTo = func(string, string) error {
		t.Fatal("healed a room that does not exist")
		return nil
	}
	t.Cleanup(func() { setRoomDefaultTo = prev })

	for _, c := range []*role.Contact{nil, {Kind: "meet"}, {Kind: "meet", Ref: "  "}} {
		if err := EnsureDefaultTo(c, role.Assignment{Kind: role.Conductor, Ref: "99"}); err != nil {
			t.Fatalf("EnsureDefaultTo(%+v) = %v, want nil", c, err)
		}
	}
}

func TestEnsureNameHealsAnExistingSprintRoomInPlace(t *testing.T) {
	var gotRef, gotName string
	prev := setRoomName
	setRoomName = func(ref, name string) error {
		gotRef, gotName = ref, name
		return nil
	}
	t.Cleanup(func() { setRoomName = prev })

	c := &role.Contact{Kind: "meet", Ref: "durable-room-id"}
	if err := EnsureName(c, role.Assignment{Kind: role.Conductor, Ref: "99"}); err != nil {
		t.Fatal(err)
	}
	if gotRef != c.Ref || gotName != "sprint 99" {
		t.Fatalf("EnsureName set (%q, %q), want (%q, %q)", gotRef, gotName, c.Ref, "sprint 99")
	}
	// The heal is not just a write to the room's persisted metadata — the
	// contact a caller already holds must reflect it too, or every surface
	// that renders this same *role.Contact keeps showing the old, nameless
	// string until something reloads it from disk.
	if c.Name != "sprint 99" {
		t.Fatalf("c.Name = %q, want %q — in-place healing must update the caller's Contact", c.Name, "sprint 99")
	}
}

// A LEGACY ROOM'S CONTACT MUST NOT GAIN A NAME IT COULD NOT PERSIST.
//
// If the underlying room store rejects the rename, the in-memory Contact
// must not silently claim a name that was never recorded — a caller that
// looks again later would find no evidence for the string it already
// printed.
func TestEnsureNameLeavesContactAloneOnFailure(t *testing.T) {
	prev := setRoomName
	setRoomName = func(ref, name string) error { return errors.New("room store unavailable") }
	t.Cleanup(func() { setRoomName = prev })

	c := &role.Contact{Kind: "meet", Ref: "durable-room-id"}
	if err := EnsureName(c, role.Assignment{Kind: role.Conductor, Ref: "99"}); err == nil {
		t.Fatal("want the store error surfaced, got nil")
	}
	if c.Name != "" {
		t.Fatalf("c.Name = %q, want empty — the rename was never persisted", c.Name)
	}
}

// ASSUME MUST HAND BACK A CONTACT THAT ALREADY CARRIES THE STABLE NAME.
//
// meet.CreateOptions.Name is what gets persisted on the room; role.Contact.Name
// is the separate field every sprint-facing surface renders through
// Contact.String(). A room opened with the right persisted name but a
// Contact missing the field would still print "meet #25 · bus conductor.126"
// — the exact gap this story exists to close.
func TestAssumeContactCarriesTheStableName(t *testing.T) {
	prev := createRoom
	createRoom = func(opts meet.CreateOptions) (*meet.State, error) {
		return &meet.State{ID: "room-1", Room: 25}, nil
	}
	t.Cleanup(func() { createRoom = prev })

	c, err := Assume(role.Assignment{Kind: role.Conductor, Ref: "126"}, "trestle")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "sprint 126" {
		t.Fatalf("Assume contact Name = %q, want %q", c.Name, "sprint 126")
	}
}

// The steward's singleton seat has no id to be stable about, so its contact
// must not pick up a synthesized name — Assume derives Contact.Name from the
// same roomNameFor every kind goes through, and roomNameFor is empty for
// role.Steward.
func TestAssumeStewardContactHasNoName(t *testing.T) {
	if got := roomNameFor(role.Assignment{Kind: role.Steward}); got != "" {
		t.Fatalf("roomNameFor(Steward) = %q, want empty", got)
	}
}
