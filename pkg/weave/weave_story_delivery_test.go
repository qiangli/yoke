package weave

import (
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

func TestStoryLifecycleNotificationsReachManagerInbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())

	sprint := &weaveStory{ID: 120, Owner: "manager"}
	for _, body := range []string{
		"worker claimed story abc",
		"worker yielded story abc",
		"worker submitted story abc",
	} {
		if err := notifySprintOwner(sprint, "worker", body); err != nil {
			t.Fatalf("notify manager: %v", err)
		}
	}

	snapshot, err := bus.SnapshotInbox("manager")
	if err != nil {
		t.Fatalf("manager inbox: %v", err)
	}
	if len(snapshot.Items) != 3 {
		t.Fatalf("manager received %d lifecycle notifications, want 3: %+v", len(snapshot.Items), snapshot.Items)
	}
	for i, verb := range []string{"claimed", "yielded", "submitted"} {
		if !strings.Contains(snapshot.Items[i].Body, verb) {
			t.Errorf("notification %d body = %q, want %q", i, snapshot.Items[i].Body, verb)
		}
		if snapshot.Items[i].Delivery != bus.DeliveryQueued {
			t.Errorf("notification %d delivery = %q, want %q", i, snapshot.Items[i].Delivery, bus.DeliveryQueued)
		}
	}
}

func TestSprintDeliveryAcceptsAttachedExternalStream(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const owner = "external-manager"
	if err := room.Join(room.Card{
		ID: room.AgentClaimID(owner), Nick: owner, Mode: "sprint-inbox",
		Tool: "agy", Binding: "agy:test", PID: os.Getpid(),
		Caps: []string{room.CapInboxStream},
	}); err != nil {
		t.Fatal(err)
	}
	defer room.Leave(room.AgentClaimID(owner))
	if !sprintInboxDeliveryLive(owner) {
		t.Fatal("live attached external inbox stream was not accepted")
	}
}

// THE BEHAVIOUR THAT CHANGED when sprintInboxDeliveryLive became a projection of
// room.OwnerTransportFor, asserted so the change is a decision with a test
// rather than something inherited from a refactor.
//
// The old private copy accepted CapInboxStream only when Mode was
// "sprint-inbox". A live `bashy inbox --watch --as X` publishes Mode "inbox",
// so it did NOT count as reachable — an agent holding its own inbox open,
// reported unreachable. It counts now: holding your inbox open IS the whole
// content of the attached rung.
func TestSprintDeliveryAcceptsAPlainInboxWatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const owner = "watching-manager"
	if err := room.Join(room.Card{
		ID: room.AgentClaimID(owner), Nick: owner, Mode: "inbox",
		Tool: "claude", Binding: "claude:test", PID: os.Getpid(),
		Caps: []string{room.CapInboxStream},
	}); err != nil {
		t.Fatal(err)
	}
	defer room.Leave(room.AgentClaimID(owner))
	if !sprintInboxDeliveryLive(owner) {
		t.Fatal("a live `inbox --watch` (Mode \"inbox\") was reported unreachable;\n" +
			"holding your own inbox open is exactly what the attached rung means")
	}
}

// And the refusal still holds where it must: a live card that promises nothing
// is not a delivery path, whatever its mode. This is the loophole
// `agents track start` produces.
func TestSprintDeliveryRefusesACardThatPromisesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const owner = "tracked-only"
	if err := room.Join(room.Card{
		ID: room.AgentClaimID(owner), Nick: owner, Mode: "weave",
		Tool: "codex", Binding: "codex:test", PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	defer room.Leave(room.AgentClaimID(owner))
	if sprintInboxDeliveryLive(owner) {
		t.Fatal("a tracked work record with no delivery capability was accepted as reachable")
	}
}

// TestSprintDeliveryGateIsConsistent pins the DECISION for todo 27ae4f3792e2,
// not the placement — which is what that story asks for.
//
// BEFORE: sprintInboxDeliveryLive REFUSED exactly one verb, `sprint focus`,
// which sets an advisory pointer and changes nothing else. The same owner could
// start, take, checkpoint and END the sprint — end being irreversible — and was
// stopped only by the cheapest, most reversible, purely bookkeeping operation.
// It cost sprint #135 its focus pointer: the refusal reads as "this seat is not
// properly established", so the conductor went looking for a problem that did
// not exist and drove the sprint without focus.
//
// AFTER: it WARNS, and does so on seating, focusing and ending alike. The
// reason is written at sprintSeatDeliveryAdvisory and is the same boundary
// sprintSeatToolMismatch already draws in this package: the host refuses what it
// can PROVE wrong (validateSprintOwner: an owner naming nobody) and reports what
// it cannot JUDGE (a human driving a sprint from a terminal is legitimate and
// has no managed session behind it).
//
// This test fails if anyone restores an asymmetry without recording a reason:
// all three verbs must treat an unwakeable owner the same way.
func TestSprintDeliveryGateIsConsistent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const owner = "unwakeable-manager"

	// No room card at all: nothing can push to this owner.
	if sprintInboxDeliveryLive(owner) {
		t.Fatal("fixture is wrong: this owner must be unreachable")
	}

	advisory := sprintSeatDeliveryAdvisory(owner)
	if strings.TrimSpace(advisory) == "" {
		t.Fatal("an unwakeable owner must still be REPORTED; a silent gap is what the story forbids")
	}
	if !strings.Contains(advisory, owner) {
		t.Errorf("the advisory must name the seat it is about, got %q", advisory)
	}

	// The advisory reaches the seating path (start/take print sprintReadyLine)
	// and the ending path, not only focus.
	ready := sprintReadyLine(136, owner)
	if !strings.Contains(ready, advisory) {
		t.Error("seating an unwakeable owner must carry the advisory: an owner nothing can push to " +
			"matters MORE when the seat is taken than when a focus pointer is moved")
	}

	// And a reachable owner must produce no advisory anywhere — the warning is
	// information, so it must not become noise on a healthy seat.
	if err := room.Join(room.Card{
		ID: room.AgentClaimID("wakeable-manager"), Nick: "wakeable-manager", Mode: "inbox",
		Tool: "claude", Binding: "claude:test", PID: os.Getpid(),
		Caps: []string{room.CapInboxStream},
	}); err != nil {
		t.Fatal(err)
	}
	defer room.Leave(room.AgentClaimID("wakeable-manager"))
	if got := sprintSeatDeliveryAdvisory("wakeable-manager"); got != "" {
		t.Errorf("a reachable seat must produce no advisory, got %q", got)
	}
}
