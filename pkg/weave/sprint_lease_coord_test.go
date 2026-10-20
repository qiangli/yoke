package weave

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/role"
	"github.com/qiangli/yoke/pkg/room"
)

func TestManagedManagerKeepsLeaseLiveDuringLongTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const holder = "manager"
	if err := room.Join(room.Card{ID: room.AgentClaimID(holder), Nick: holder,
		PID: os.Getpid(), Caps: []string{room.CapInboxDelivery}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	s := &weaveStory{ID: 98, Lease: &weaveStoryLease{Holder: holder,
		At: now.Add(-SprintLeaseTTL - time.Minute)}}
	if got := s.seatAt(now).Live(now); got != role.LivenessLive {
		t.Fatalf("live managed manager with an old sprint verb has lease %s, want live", got)
	}
	s.Lease.At = time.Now().Add(-SprintLeaseTTL - time.Minute)
	if err := sprintLeaseAcquire(s, "successor", false); err == nil {
		t.Fatal("successor recovered a lease held by a live managed manager")
	}
}

func TestDeadManagedManagerLeaseCanBeRecovered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	if err := room.Join(room.Card{ID: room.AgentClaimID("former"), Nick: "former",
		PID: retiredPID(t), Caps: []string{room.CapInboxDelivery}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	s := &weaveStory{ID: 98, Lease: &weaveStoryLease{Holder: "former",
		At: now.Add(-SprintLeaseTTL - time.Minute)}}
	if got := s.seatAt(now).Live(now); got != role.LivenessLapsed {
		t.Fatalf("dead manager lease = %s, want lapsed", got)
	}
	// An old beat without a live room member must remain recoverable.
	s.Lease.At = time.Now().Add(-SprintLeaseTTL - time.Minute)
	if err := sprintLeaseAcquire(s, "successor", false); err != nil {
		t.Fatalf("successor could not recover dead manager lease: %v", err)
	}
	if s.Lease.Holder != "successor" {
		t.Fatalf("holder = %q, want successor", s.Lease.Holder)
	}
}

func mutateSprintCard(t *testing.T, id int64, fn func(s *weaveStory)) {
	t.Helper()
	dir, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			t.Fatalf("sprint #%d not found", id)
		}
		fn(s)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTakeRefusesAnUnknownSeatWithoutForce: a lease with a holder and no
// heartbeat is UNKNOWN, not stale — nothing says the holder is gone — so a
// take must refuse it, and --force must still be able to move it.
func TestTakeRefusesAnUnknownSeatWithoutForce(t *testing.T) {
	takeFixture(t, "agent-a")
	if out, code := runSprint(t, "take", "1", "--owner", "agent-a"); code != 0 {
		t.Fatalf("first take: %s", out)
	}
	mutateSprintCard(t, 1, func(s *weaveStory) { s.Lease.At = time.Time{} })

	seedAgent(t, "agent-b")
	t.Setenv("BASHY_PRINCIPAL", "agent-b")
	out, code := runSprint(t, "take", "1", "--owner", "agent-b")
	if code == 0 {
		t.Fatalf("take succeeded over an unknown seat without --force:\n%s", out)
	}
	if !strings.Contains(out, "agent-a") || !strings.Contains(out, "--force") {
		t.Errorf("refusal must name the holder and the way through:\n%s", out)
	}
	held, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if held.Lease == nil || held.Lease.Holder != "agent-a" {
		t.Fatalf("a refused take moved the seat: %+v", held.Lease)
	}

	if out, code := runSprint(t, "take", "1", "--owner", "agent-b", "--force"); code != 0 {
		t.Fatalf("--force take of an unknown seat: %s", out)
	}
}

// TestSprintLeaseEpochMovesOnlyWhenTheSeatChangesHands: the epoch is a fencing
// token. A re-take or heartbeat by the holder keeps it, a new holder bumps it,
// a vacated seat never reissues an epoch, and a holder carrying a superseded
// epoch is refused.
func TestSprintLeaseEpochMovesOnlyWhenTheSeatChangesHands(t *testing.T) {
	takeFixture(t, "agent-a")
	epoch := func() uint64 {
		t.Helper()
		s, err := sprintOwnerSnapshot(1)
		if err != nil {
			t.Fatal(err)
		}
		return s.LeaseEpoch
	}

	if out, code := runSprint(t, "take", "1", "--owner", "agent-a"); code != 0 {
		t.Fatalf("take: %s", out)
	}
	first := epoch()
	if first == 0 {
		t.Fatal("a take recorded no epoch")
	}
	if out, code := runSprint(t, "take", "1", "--owner", "agent-a"); code != 0 {
		t.Fatalf("re-take: %s", out)
	}
	if got := epoch(); got != first {
		t.Fatalf("re-taking one's own seat moved the epoch: %d -> %d", first, got)
	}

	seedAgent(t, "agent-b")
	t.Setenv("BASHY_PRINCIPAL", "agent-b")
	if out, code := runSprint(t, "take", "1", "--owner", "agent-b", "--force"); code != 0 {
		t.Fatalf("force take: %s", out)
	}
	second := epoch()
	if second <= first {
		t.Fatalf("a new holder did not bump the epoch: %d -> %d", first, second)
	}

	// The old holder's epoch is fenced: it cannot heartbeat the seat back.
	mutateSprintCard(t, 1, func(s *weaveStory) {
		err := withSprintCard(s, func() error {
			_, err := coord.Refresh(t.Context(), sprintLeaseRef(s.ID), principal.Ref{Name: "agent-b"}, first)
			return err
		})
		if !errors.Is(err, coord.ErrFenced) && !errors.Is(err, coord.ErrEpochMismatch) {
			t.Errorf("a stale epoch was not refused: %v", err)
		}
		err = withSprintCard(s, func() error {
			_, err := coord.Refresh(t.Context(), sprintLeaseRef(s.ID), principal.Ref{Name: "agent-a"}, second)
			return err
		})
		if err == nil {
			t.Errorf("a superseded holder refreshed a seat it no longer holds")
		}
	})
	if got := epoch(); got != second {
		t.Fatalf("a refused refresh moved the epoch: %d -> %d", second, got)
	}

	// Vacating keeps the high-water mark, so the next occupant outranks both.
	if out, code := runSprint(t, "handoff", "1", "-m", "done for now"); code != 0 {
		t.Fatalf("handoff: %s", out)
	}
	if got := epoch(); got != second {
		t.Fatalf("a vacated seat lost its epoch: %d -> %d", second, got)
	}
	if out, code := runSprint(t, "take", "1", "--owner", "agent-a"); code != 0 {
		t.Fatalf("take of a vacant seat: %s", out)
	}
	if got := epoch(); got <= second {
		t.Fatalf("the epoch was reissued after a vacancy: %d -> %d", second, got)
	}
}
