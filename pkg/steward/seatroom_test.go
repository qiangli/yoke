package steward

import (
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/role"
)

// The seam is a package var wired by the host, so the interesting property is
// that it is USED — and that its absence and its failure are both reported
// rather than mistaken for success.
func TestSeatRoom_HookIsUsedAndFailuresAreReported(t *testing.T) {
	t.Setenv("BASHY_STEWARD_DIR", t.TempDir())
	old, oldClose := OpenRoom, CloseRoom
	t.Cleanup(func() { OpenRoom, CloseRoom = old, oldClose })

	// Unwired: no room, and nothing pretends otherwise.
	OpenRoom, CloseRoom = nil, nil
	if got := assumeSeatRoom("tester"); got != "" {
		t.Errorf("with no hook the seat must claim no room, got %q", got)
	}

	// Wired and failing: REPORTED. A steward that could not open a room still
	// holds the seat, and the surface must say the intercom is down rather
	// than imply a channel.
	OpenRoom = func(role.Assignment, string) (*role.Contact, error) {
		return nil, errors.New("meet unavailable")
	}
	got := assumeSeatRoom("tester")
	if !strings.Contains(got, "no room") || !strings.Contains(got, "meet unavailable") {
		t.Errorf("a failed open must name the reason, got %q", got)
	}
	if !strings.Contains(got, "bus steward.") {
		t.Errorf("a failed open should still give the bus topic, got %q", got)
	}

	// Wired and working: the address is recorded, so release can find it.
	OpenRoom = func(a role.Assignment, holder string) (*role.Contact, error) {
		if a.Kind != role.Steward {
			t.Errorf("assignment kind = %q, want steward", a.Kind)
		}
		return &role.Contact{Kind: "meet", Ref: "room-abc", Room: 7, Topic: a.Topic()}, nil
	}
	if got := assumeSeatRoom("tester"); !strings.Contains(got, "meet #7") {
		t.Errorf("a successful open must report where, got %q", got)
	}
	if c, err := loadSeatContact(); err != nil || c == nil || c.Ref != "room-abc" {
		t.Fatalf("the address must be recorded for release to find: %+v %v", c, err)
	}

	// Release closes it and forgets the address — a released seat whose room
	// stayed open would advertise a channel to somebody who stepped away.
	var closed string
	CloseRoom = func(c *role.Contact, holder string) error { closed = c.Ref; return nil }
	if got := releaseSeatRoom("tester"); !strings.Contains(got, "closed") {
		t.Errorf("release should report the close, got %q", got)
	}
	if closed != "room-abc" {
		t.Errorf("CloseRoom got %q, want room-abc", closed)
	}
	if c, _ := loadSeatContact(); c != nil {
		t.Error("the address must be forgotten after release")
	}
}

// A STEWARD'S ADDRESS MUST DISTINGUISH ONE MACHINE'S STEWARD FROM ANOTHER'S.
//
// A steward manages the resources of one login on one host, so the same person
// logged into two machines has two stewards with two journals. An address keyed
// on the bare login would be ambiguous between them, and a message meant for
// the work on one machine would reach whichever listener answered first.
//
// (The user's TWIN — the cloudbox-account identity that manages those stewards
// — is a level up and is not built. See docs/agent-identity-model.md.)
func TestStewardAddress_DistinguishesMachines(t *testing.T) {
	topic := stewardAssignment().Topic()
	if !strings.HasPrefix(topic, "steward.") {
		t.Fatalf("topic = %q, want a steward topic", topic)
	}
	sc, err := OSScope{}.Scope()
	if err != nil {
		t.Skip("no scope on this host")
	}
	if !strings.Contains(topic, sc.ID) {
		t.Errorf("topic %q must carry the scope id %q — otherwise two machines' "+
			"stewards share one address", topic, sc.ID)
	}
	if stewardAssignment().Topic() != topic {
		t.Error("the address must be deterministic")
	}
}
