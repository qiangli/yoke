package room

// One live owning session per claim id, decided on the session digest and the
// STABLE owner process. Each test here is one of the three outcomes
// ClaimSession has to get right — heartbeat, competing session, stale
// incumbent — plus the release fencing that protects them.

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// deadPID is a pid that has certainly exited: a reaped child.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child to reap: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	if PidAlive(pid) {
		t.Skipf("pid %d still reports alive after reaping", pid)
	}
	return pid
}

func sessionCard(id, digest string, ownerPID, pid int) Card {
	return Card{ID: id, Tool: "claude", Binding: "claude:opus5.5",
		SessionClaim: digest, OwnerPID: ownerPID, PID: pid, Mode: "interactive"}
}

// A per-turn child command is the SAME session: equal digest, different pid,
// and the claim is kept rather than contested.
func TestClaimSessionAcceptsTheSameSessionFromAnotherPid(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	id := "instance:11111111-2222-3333-4444-555555555555"

	if err := ClaimSession(sessionCard(id, "digest-a", os.Getpid(), os.Getpid())); err != nil {
		t.Fatal(err)
	}
	first, ok := SessionOwner(id)
	if !ok {
		t.Fatal("the claim did not take")
	}
	// A later turn: same digest, a pid that has already exited.
	if err := ClaimSession(sessionCard(id, "digest-a", os.Getpid(), deadPID(t))); err != nil {
		t.Fatalf("the same session was refused its own claim: %v", err)
	}
	second, ok := SessionOwner(id)
	if !ok {
		t.Fatal("the session lost its claim to its own heartbeat")
	}
	if second.Joined != first.Joined {
		t.Errorf("a heartbeat rewrote Joined: %q -> %q", first.Joined, second.Joined)
	}
}

// THE FINDING. A live incumbent whose last turn's child pid has exited is
// still live, because the OWNER process is. Judging on Card.PID made the
// incumbent look dead between turns, so a competing session could take over a
// conversation that was still in progress — the refusal was only in force
// while a child command happened to be running.
func TestClaimSessionRefusesACompetitorBetweenTheIncumbentsTurns(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	id := "instance:11111111-2222-3333-4444-555555555555"

	incumbent := sessionCard(id, "digest-a", os.Getpid(), deadPID(t))
	if err := ClaimSession(incumbent); err != nil {
		t.Fatal(err)
	}
	err := ClaimSession(sessionCard(id, "digest-b", os.Getpid(), os.Getpid()))
	var live *ErrLive
	if !errors.As(err, &live) {
		t.Fatalf("a competing session was granted the claim: %v", err)
	}
	// The incumbent's card is intact and still reports it as the owner.
	owner, ok := SessionOwner(id)
	if !ok || owner.SessionClaim != "digest-a" {
		t.Fatalf("the refusal did not leave the incumbent in place: %+v %v", owner, ok)
	}
}

// A card whose OWNER process is gone is stale, and a newcomer reclaims it.
// Reading is the reconciliation; there is still no sweeper.
func TestClaimSessionReclaimsAStaleIncumbent(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	id := "instance:11111111-2222-3333-4444-555555555555"
	gone := deadPID(t)

	if err := ClaimSession(sessionCard(id, "digest-a", gone, gone)); err != nil {
		t.Fatal(err)
	}
	if _, ok := SessionOwner(id); ok {
		t.Fatal("a card with a dead owner still reports a live owner")
	}
	if err := ClaimSession(sessionCard(id, "digest-b", os.Getpid(), os.Getpid())); err != nil {
		t.Fatalf("a stale claim was not reclaimable: %v", err)
	}
	owner, ok := SessionOwner(id)
	if !ok || owner.SessionClaim != "digest-b" {
		t.Fatalf("owner after reclaim = %+v, %v", owner, ok)
	}
}

// A live incumbent that predates this contract carries no digest. It is
// refused rather than inherited: there is no evidence it is us, and guessing
// in favour of the newcomer hands a live conversation to a second driver.
func TestClaimSessionRefusesALiveIncumbentWithNoDigest(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	id := "legacy-agent"
	legacy := Card{ID: id, Tool: "claude", Binding: "claude:opus5.5", PID: os.Getpid()}
	if err := Join(legacy); err != nil {
		t.Fatal(err)
	}
	var live *ErrLive
	if err := ClaimSession(sessionCard(id, "digest-b", os.Getpid(), os.Getpid())); !errors.As(err, &live) {
		t.Fatalf("a live pre-contract member was overwritten: %v", err)
	}
}

// A claim with no digest is refused outright: an empty digest compares equal
// to every other empty digest, so defaulting would make all unattributed
// sessions one session.
func TestClaimSessionRequiresADigest(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	if err := ClaimSession(Card{ID: "x", Tool: "claude"}); !errors.Is(err, ErrNoSessionClaim) {
		t.Fatalf("got %v", err)
	}
	if err := ClaimSession(Card{SessionClaim: "d", Tool: "claude"}); err == nil {
		t.Fatal("a claim with no id was accepted")
	}
}

// Release is fenced on the digest, so a session cannot release a claim it does
// not hold — including the case the lock exists for, where the id has changed
// hands since the releasing session last saw it.
func TestReleaseSessionOnlyReleasesItsOwnClaim(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	id := "instance:11111111-2222-3333-4444-555555555555"
	if err := ClaimSession(sessionCard(id, "digest-a", os.Getpid(), os.Getpid())); err != nil {
		t.Fatal(err)
	}
	ReleaseSession(id, "digest-b")
	if _, ok := SessionOwner(id); !ok {
		t.Fatal("a foreign digest released somebody else's claim")
	}
	ReleaseSession(id, "digest-a")
	if _, ok := SessionOwner(id); ok {
		t.Fatal("the owning session could not release its own claim")
	}
	// Releasing what you do not hold is a no-op, not an error: every caller
	// defers a release that runs even when the claim was refused.
	ReleaseSession(id, "digest-a")
}
