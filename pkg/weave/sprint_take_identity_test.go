package weave

// `sprint take` CHECKS BEFORE IT WRITES.
//
// The seat is the one place where "which instance is conducting" is decided,
// and the private lease token is the credential that proves the decision. The
// candidate minted and SAVED that token and only then asked whether the caller
// was allowed to take the seat, so a refused competing session left a valid
// token file behind for a lease it does not hold. Its own comment said "before
// anything is written".

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// takeFixture is one sprint in a private store, owned by one registered agent.
func takeFixture(t *testing.T, owner string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", "")
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	t.Setenv("BASHY_PRINCIPAL", owner)
	seedAgent(t, owner)
	if out, code := runSprint(t, "add", "identity fixture"); code != 0 {
		t.Fatalf("add: %s", out)
	}
}

// leaseTokenExists reports whether the private credential file for a holder is
// on disk.
func leaseTokenExists(t *testing.T, id int64, holder string) bool {
	t.Helper()
	path, err := sprintLeaseTokenPath(id, holder)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(path)
	return err == nil
}

// A REFUSED take leaves no credential behind.
//
// Two sessions of one instance: the first takes the seat, the second is the
// competing driver the instance contract refuses. The refusal is not in
// question here — what is, is that the refused caller must not be holding a
// token that authorizes the very checkpoint/handoff verbs the refusal denied
// it.
func TestRefusedSprintTakeLeavesNoLeaseToken(t *testing.T) {
	const owner = "agent-a"
	takeFixture(t, owner)
	t.Setenv("BASHY_INSTANCE_UUID", "11111111-2222-5333-8444-555555555555")
	t.Setenv("BASHY_SESSION_CLAIM", "sha256:first-session")
	if out, code := runSprint(t, "take", "1", "--owner", owner); code != 0 {
		t.Fatalf("first take: %s", out)
	}
	held, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	heldHash := held.Lease.TokenHash

	// A SECOND owning session of the SAME instance: a second driver of one
	// conversation, which is not a handoff. Take it under a different holder
	// name so the token path it would orphan is its own.
	const competitor = "agent-b"
	seedAgent(t, competitor)
	t.Setenv("BASHY_PRINCIPAL", competitor)
	t.Setenv("BASHY_SESSION_CLAIM", "sha256:second-session")
	out, code := runSprint(t, "take", "1", "--owner", competitor, "--force")
	if code == 0 {
		t.Fatalf("a competing session of the holding instance took the seat: %s", out)
	}
	if !strings.Contains(out, "another live session of instance") {
		t.Fatalf("refusal did not name the competing-session rule: %s", out)
	}
	if leaseTokenExists(t, 1, competitor) {
		t.Fatal("the refused session was left holding a private lease token for a seat it does not hold")
	}
	after, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lease.Holder != owner || after.Lease.TokenHash != heldHash {
		t.Fatalf("the refused take moved the seat: %+v", after.Lease)
	}
}

// Crashed-owner recovery is still REAL.
//
// The repair must not buy "refuse a competing session" at the price of "never
// recover a seat whose conductor died". A lapsed lease has no live driver to
// compete with, so the same instance resuming on a fresh session takes its own
// seat back.
func TestStaleSeatIsRecoverableByTheSameInstanceOnANewSession(t *testing.T) {
	const owner = "agent-a"
	takeFixture(t, owner)
	const instance = "99999999-8888-5777-8666-555555555555"
	t.Setenv("BASHY_INSTANCE_UUID", instance)
	t.Setenv("BASHY_SESSION_CLAIM", "sha256:crashed-session")
	if out, code := runSprint(t, "take", "1", "--owner", owner); code != 0 {
		t.Fatalf("first take: %s", out)
	}

	// The conductor died; its lease lapses.
	if err := withWeaveQueueLock(sprintDirForTest(t), func(q *weaveQueue) error {
		s := findWeaveStory(q, 1)
		s.Lease.At = time.Now().UTC().Add(-10 * SprintLeaseTTL)
		s.Lease.AttachedPID = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The same instance resumes on a new session and recovers its seat.
	t.Setenv("BASHY_SESSION_CLAIM", "sha256:resumed-session")
	if out, code := runSprint(t, "take", "1", "--owner", owner); code != 0 {
		t.Fatalf("crashed-owner recovery was refused: %s", out)
	}
	after, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lease.Session != "sha256:resumed-session" || after.Lease.Instance != instance {
		t.Fatalf("recovery did not record the resuming session: %+v", after.Lease)
	}
}

// A holder that is an active deputy over the sprint cannot take its conductor
// lease: a deputy may activate, fence, judge and gate inside its scope, but it
// never conducts what it supervises. The verdict comes from the steward seat;
// here it is stated so the test does not need one.
func TestTakeRefusesActiveDeputyHolder(t *testing.T) {
	const owner = "agent-a"
	takeFixture(t, owner)
	prev := vetSprintConductor
	vetSprintConductor = func(id int64, holder, epic string) error {
		if id == 1 && holder == owner {
			return fmt.Errorf("sprint #%d conductor %q holds an active deputy over it — a deputy never conducts what it supervises", id, holder)
		}
		return nil
	}
	t.Cleanup(func() { vetSprintConductor = prev })
	out, code := runSprint(t, "take", "1", "--owner", owner)
	if code == 0 {
		t.Fatalf("a deputy holder took the conductor lease of its own sprint: %s", out)
	}
	if !strings.Contains(out, "deputy") {
		t.Fatalf("the refusal did not name the deputy rule: %s", out)
	}
	after, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(after.Owner) != "" || after.Lease != nil {
		t.Fatalf("the refused take moved the seat: owner %q lease %+v", after.Owner, after.Lease)
	}
}

// Without a steward seat there are no deputies, so the check allows: the
// appointment path must not depend on a seat that was never claimed.
func TestVetSprintConductorAllowsWithoutSeat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_STEWARD_DIR", "")
	if err := defaultVetSprintConductor(1, "agent-a", ""); err != nil {
		t.Fatalf("no seat must allow: %v", err)
	}
}

func sprintDirForTest(t *testing.T) string {
	t.Helper()
	dir, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
