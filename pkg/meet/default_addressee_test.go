package meet

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

// withSeat installs a host role table for the duration of one test. bus's own
// registry composes and cannot be unregistered, so a test swaps the whole
// function and restores it.
func withSeat(t *testing.T, label, topic, holder string) {
	t.Helper()
	prev := bus.HostRoles
	bus.HostRoles = func() []bus.HostRole {
		return []bus.HostRole{{Label: label, Topic: topic, Holder: holder}}
	}
	t.Cleanup(func() { bus.HostRoles = prev })
}

// An unaddressed post in a room that declares a default addressee is addressed
// to the SEAT. Before this, a question asked in a sprint's own room named
// nobody, so nobody was accountable for answering it — which is how four
// operator questions sat unread for five hours under a healthy owner mark.
func TestPostAsAddressesTheRoomsDefaultSeat(t *testing.T) {
	st := newRoom(t)
	st.DefaultTo = "conductor:99"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	withSeat(t, "conductor:99", "conductor.99", "trestle")

	ev, err := PostAs(st.ID, "qiangli", "", "what is the plan?")
	if err != nil {
		t.Fatal(err)
	}
	if ev.To != "conductor:99" {
		t.Fatalf("To = %q, want the room's late-bound seat label", ev.To)
	}
}

// THE INVARIANT: the stored address is the seat, never its holder. Resolution
// happens at READ time, so a handover re-targets mail already written rather
// than orphaning it against a name nobody answers to any more.
func TestSeatAddressedMailFollowsTheSeatAcrossAHandover(t *testing.T) {
	st := newRoom(t)
	st.DefaultTo = "conductor:99"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	withSeat(t, "conductor:99", "conductor.99", "trestle")
	if _, err := PostAs(st.ID, "qiangli", "", "who owns this?"); err != nil {
		t.Fatal(err)
	}

	directed, _, _, _, err := UnreadRecords(st.ID, "trestle", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 1 {
		t.Fatalf("the seat's holder got %d directed records, want 1", len(directed))
	}

	// The lease changes hands. Nothing in the transcript is rewritten.
	withSeat(t, "conductor:99", "conductor.99", "keystone")
	if directed, _, _, _, err = UnreadRecords(st.ID, "keystone", 0); err != nil {
		t.Fatal(err)
	}
	if len(directed) != 1 {
		t.Fatalf("the successor got %d directed records, want the in-flight message", len(directed))
	}
	if directed, _, _, _, err = UnreadRecords(st.ID, "trestle", 0); err != nil {
		t.Fatal(err)
	}
	if len(directed) != 0 {
		t.Fatalf("the former holder is still addressed by %d record(s)", len(directed))
	}
}

// An explicit --to still wins over the room default: the resolution ladder's
// first rule is the explicit agent.
func TestExplicitAddresseeBeatsTheRoomDefault(t *testing.T) {
	st := newRoom(t)
	st.DefaultTo = "conductor:99"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	withSeat(t, "conductor:99", "conductor.99", "trestle")

	ev, err := PostAs(st.ID, "qiangli", "codex", "for you specifically")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(ev.To, "codex") {
		t.Fatalf("To = %q, want the explicitly named agent", ev.To)
	}
}

// A room with no declared default keeps the old behavior: unaddressed is
// unaddressed. The default addressee is a property of ROLE rooms, not of every
// meeting somebody opens.
func TestPostAsLeavesAnOrdinaryRoomUnaddressed(t *testing.T) {
	st := newRoom(t)
	ev, err := PostAs(st.ID, "qiangli", "", "just thinking out loud")
	if err != nil {
		t.Fatal(err)
	}
	if ev.To != "" {
		t.Fatalf("To = %q, want an unaddressed contribution", ev.To)
	}
}
