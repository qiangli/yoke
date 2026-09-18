package bus

import (
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

// THE DEFECT, stated as a test: a notification addressed to a role's topic must
// be MATCHED by that role's inbox.
//
// Before EnsureRoleInbox, `steward ping` addressed `To: Holder.Name` — a TOOL
// name (`codex`) — while every subscriber was an AGENT name
// (`codex-gpt5.6-sol`). Matches accepts on To, Room or Topics and missed all
// three at once, so the ping was published, stored, durable and unreadable.
//
// That is the worst shape a message store has: not lost, so nothing reports it;
// not deliverable, so nobody gets it.
func TestEnsureRoleInbox_MatchesNotificationsAddressedToTheRole(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const topic = "steward.dragon-u501-b683b300b1"

	created, err := EnsureRoleInbox(topic)
	if err != nil {
		t.Fatalf("EnsureRoleInbox: %v", err)
	}
	if !created {
		t.Fatal("first call did not create the inbox")
	}

	sub, err := LoadSubscription(topic)
	if err != nil {
		t.Fatalf("LoadSubscription: %v", err)
	}

	// Addressed 1:1 to the role — what a ping sends.
	if !sub.Matches(room.Event{Type: room.EventNotify, To: topic}) {
		t.Error("a ping addressed TO the role does not match its own inbox")
	}
	// Addressed to the topic — what a broadcast to the role sends.
	if !sub.Matches(room.Event{Type: room.EventNotify, Topic: topic}) {
		t.Error("a notification on the role's TOPIC does not match its own inbox")
	}
	// The regression itself: the holder's tool name must not be the address.
	if sub.Matches(room.Event{Type: room.EventNotify, To: "codex"}) {
		t.Error("the inbox accepts a TOOL name — the address must be the role, not its holder")
	}

	// An inbox, not a doorbell: no interrupt rights granted by merely existing.
	if len(sub.InterruptFrom) != 0 {
		t.Errorf("role inbox granted interrupt rights to %v — a role is a bigger responsibility, not a bigger permission", sub.InterruptFrom)
	}
	// Not joined to the public board: messages sent TO the role must not be
	// buried under everything sent to everyone.
	if sub.Room != "" {
		t.Errorf("role inbox joined room %q — it is an address, not a board member", sub.Room)
	}
}

// A role inbox opens at 0, NOT at the timeline head — the opposite of an
// agent's mailbox, and deliberately.
//
// EnsureSubscription opens an agent at the head because nothing addressed to it
// could exist before it had an address. A role is the reverse: pings were
// published to this topic while it had no inbox, and they are still in the
// timeline. Starting at the head would skip exactly the messages the fix exists
// to deliver.
func TestEnsureRoleInbox_DeliversTheBacklogItCouldNotReadBefore(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const topic = "steward.host-1"

	if _, err := EnsureRoleInbox(topic); err != nil {
		t.Fatalf("EnsureRoleInbox: %v", err)
	}
	sub, err := LoadSubscription(topic)
	if err != nil {
		t.Fatalf("LoadSubscription: %v", err)
	}
	if sub.Since != 0 {
		t.Fatalf("role inbox opened at %d — a backlog published before the inbox existed would be skipped", sub.Since)
	}
}

// Idempotent, because the send path calls it too: a ping must not depend on a
// claim having run under an earlier version that never opened the inbox. And an
// operator's tuning must survive the repair.
func TestEnsureRoleInbox_IsIdempotentAndPreservesPolicy(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const topic = "conductor.22"

	if _, err := EnsureRoleInbox(topic); err != nil {
		t.Fatalf("EnsureRoleInbox: %v", err)
	}
	// An operator grants interrupt rights and a rate cap.
	sub, _ := LoadSubscription(topic)
	sub.InterruptFrom = []string{"dhnt:human/alice"}
	sub.MaxPerMin = 3
	if err := SaveSubscription(sub); err != nil {
		t.Fatalf("SaveSubscription: %v", err)
	}

	created, err := EnsureRoleInbox(topic)
	if err != nil {
		t.Fatalf("second EnsureRoleInbox: %v", err)
	}
	if created {
		t.Error("reported creating an inbox that already existed")
	}

	got, _ := LoadSubscription(topic)
	if len(got.InterruptFrom) != 1 || got.InterruptFrom[0] != "dhnt:human/alice" {
		t.Errorf("InterruptFrom reset to %v — a reconcile that silently reopens the governance boundary is a security regression", got.InterruptFrom)
	}
	if got.MaxPerMin != 3 {
		t.Errorf("MaxPerMin reset to %d, want the operator's 3", got.MaxPerMin)
	}
}

// The read path branches on this, so a wrong answer either strands a role's
// backlog (agent treatment for a role) or replays an agent's whole history
// (role treatment for an agent).
func TestIsRoleName(t *testing.T) {
	for name, want := range map[string]bool{
		"steward.dragon-u501-b683b300b1": true,
		"conductor.22":                   true,
		"codex-gpt5.6-sol":               false, // an agent: no dot at all
		"agy-gemini3.6-flash":            false,
		"qiangli":                        false, // a human's login
		"":                               false,
		"steward":                        false, // the bare kind is not an address
		// A model name is dotted and must not be mistaken for a role.
		"ycode-glm-5.2": false,
		// Not a role kind, however dotted.
		"code.api.Foo": false,
		// `worker` is deliberately excluded: a worker is addressed through the
		// conductor that owns it, so an inbox here would be an address nothing
		// sends to.
		"worker.7": false,
	} {
		if got := IsRoleName(name); got != want {
			t.Errorf("IsRoleName(%q) = %v, want %v", name, got, want)
		}
	}
}

// A pre-existing subscription written before role inboxes existed is REPAIRED
// additively, so a seat claimed by an older build becomes reachable without an
// operator noticing anything was wrong.
func TestEnsureRoleInbox_RepairsAnUnaddressableSubscription(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const topic = "steward.host-2"

	// What an older build could leave behind: a record with the name but no way
	// for anything to match it.
	if err := SaveSubscription(Subscription{Subscriber: topic}); err != nil {
		t.Fatalf("SaveSubscription: %v", err)
	}
	if _, err := EnsureRoleInbox(topic); err != nil {
		t.Fatalf("EnsureRoleInbox: %v", err)
	}
	sub, _ := LoadSubscription(topic)
	if !sub.Matches(room.Event{Type: room.EventNotify, To: topic}) {
		t.Fatal("an existing but unaddressable subscription was not repaired")
	}
}
