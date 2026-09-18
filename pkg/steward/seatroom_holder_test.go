package steward

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/role"
)

// REGRESSION (found by a live `steward start` → `stop` cycle): a room convened
// by a PREVIOUS holder must not be reused.
//
// EnsureRoom exists so one seat never advertises two rooms. The tempting reading
// of that — "a saved contact means a room exists, reuse it" — produced the worst
// available outcome the first time a second steward started against a
// predecessor's room: meet lets any member post but only the ORGANIZER change
// the roster, so `steward stop` reported
//
//	room ... could not be closed (only the organizer may change the roster)
//
// and the host went on advertising a live channel to a seat nobody held. An
// abandoned room is more damaging than an absent one, because it still looks
// answerable and costs the time of whoever trusts it.
func TestEnsureRoom_DoesNotReuseAnotherHoldersRoom(t *testing.T) {
	t.Setenv("BASHY_STEWARD_DIR", t.TempDir())

	old, oldClose := OpenRoom, CloseRoom
	t.Cleanup(func() { OpenRoom, CloseRoom = old, oldClose })

	var opened []string
	OpenRoom = func(a role.Assignment, holder string) (*role.Contact, error) {
		opened = append(opened, holder)
		return &role.Contact{Kind: "meet", Ref: "room-" + holder, Room: len(opened), Topic: a.Topic(), Holder: holder}, nil
	}
	CloseRoom = func(*role.Contact, string) error { return nil }

	// First holder opens a room.
	if line := EnsureRoom("alice"); !strings.Contains(line, "room-alice") && !strings.Contains(line, "reachable") {
		t.Fatalf("alice got no room: %q", line)
	}
	if len(opened) != 1 {
		t.Fatalf("expected one room opened, got %v", opened)
	}

	// Same holder again: REUSE. One seat, one room.
	if line := EnsureRoom("alice"); !strings.Contains(line, "already reachable") {
		t.Errorf("alice's own room was not reused: %q", line)
	}
	if len(opened) != 1 {
		t.Errorf("a second room was opened for the same holder: %v", opened)
	}

	// A DIFFERENT holder: open a fresh one it can actually close.
	EnsureRoom("bob")
	if len(opened) != 2 || opened[1] != "bob" {
		t.Errorf("bob reused alice's room — he cannot close it, so the seat would advertise "+
			"a channel it can never retire (opened=%v)", opened)
	}
}

// A contact written before Holder existed has no holder recorded. Treat it as
// reusable, which is exactly what it was: refusing it would churn a room on
// every start for no gain.
func TestEnsureRoom_ReusesAPreFieldContact(t *testing.T) {
	t.Setenv("BASHY_STEWARD_DIR", t.TempDir())

	old, oldClose := OpenRoom, CloseRoom
	t.Cleanup(func() { OpenRoom, CloseRoom = old, oldClose })
	opens := 0
	OpenRoom = func(a role.Assignment, holder string) (*role.Contact, error) {
		opens++
		return &role.Contact{Kind: "meet", Ref: "r", Topic: a.Topic()}, nil // no Holder
	}
	CloseRoom = func(*role.Contact, string) error { return nil }

	EnsureRoom("alice")
	EnsureRoom("bob")
	if opens != 1 {
		t.Errorf("a holder-less contact was not reused: %d rooms opened", opens)
	}
}

func TestEnsureRoom_ReensuresPermanentSeatForNewHolder(t *testing.T) {
	t.Setenv("BASHY_STEWARD_DIR", t.TempDir())

	old, oldClose := OpenRoom, CloseRoom
	t.Cleanup(func() { OpenRoom, CloseRoom = old, oldClose })
	var opened []string
	OpenRoom = func(a role.Assignment, holder string) (*role.Contact, error) {
		opened = append(opened, holder)
		return &role.Contact{Kind: "meet-permanent", Ref: "steward-room", Room: 1, Topic: a.Topic()}, nil
	}
	CloseRoom = func(*role.Contact, string) error { return nil }

	EnsureRoom("alice")
	EnsureRoom("bob")
	if len(opened) != 2 || opened[0] != "alice" || opened[1] != "bob" {
		t.Fatalf("permanent room was not re-ensured across handoff: %v", opened)
	}
	c, err := SeatContact()
	if err != nil || c == nil || c.Ref != "steward-room" {
		t.Fatalf("saved permanent contact = %+v, %v", c, err)
	}
}
