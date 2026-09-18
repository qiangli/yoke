package weave

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// seedSprintLease puts one held sprint in a private store and returns a reader
// for its lease.
func seedSprintLease(t *testing.T, holder string) func() weaveStoryLease {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", dir)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Stories = append(q.Stories, &weaveStory{
			ID: 98, Title: "held sprint", Column: "doing",
			Lease: &weaveStoryLease{Holder: holder, At: time.Now().UTC()},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return func() weaveStoryLease {
		q, err := readWeaveQueue(dir)
		if err != nil {
			t.Fatal(err)
		}
		s := findWeaveStory(q, 98)
		if s == nil || s.Lease == nil {
			t.Fatalf("sprint #98 lost its lease: %#v", s)
		}
		return *s.Lease
	}
}

// A process that will still be running when its own beat is read back has to
// SAY SO. Without the pid on the lease, `bashy agent` could not tell a live
// attached watch from one killed a second after its last beat, and reported
// the dead one healthy for the rest of the TTL.
func TestHoldSprintManagerLeaseRecordsTheHoldingProcess(t *testing.T) {
	lease := seedSprintLease(t, "codex")
	if err := HoldSprintManagerLease(98, "codex", 4242); err != nil {
		t.Fatal(err)
	}
	if got := lease().AttachedPID; got != 4242 {
		t.Fatalf("AttachedPID = %d, want 4242", got)
	}
}

// An inbox read is an EVENT, not a tenancy. It must clear any pid a previous
// attached watch left behind — otherwise the refreshed seat would be graded
// against a process the refresher never was, and die on the spot.
func TestRefreshingByActivityClearsTheHoldingProcess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh func(*testing.T)
	}{
		{"manager refresh", func(t *testing.T) {
			if err := RefreshSprintManagerLease(98, "codex"); err != nil {
				t.Fatal(err)
			}
		}},
		{"owner read its mail", func(t *testing.T) { RefreshSprintOwnerActivity("codex") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := seedSprintLease(t, "codex")
			if err := HoldSprintManagerLease(98, "codex", 4242); err != nil {
				t.Fatal(err)
			}
			tc.refresh(t)
			got := lease()
			if got.AttachedPID != 0 {
				t.Fatalf("AttachedPID = %d, want 0: an ephemeral refresher holds no process", got.AttachedPID)
			}
			if got.At.IsZero() {
				t.Fatal("the refresh did not record activity at all")
			}
		})
	}
}

// Detaching retracts the CLAIM TO BE BREATHING and nothing else. The holder
// stays on the record so a successor can see whose seat it was, and the seat
// reads unheld immediately instead of coasting on a beat with nothing behind
// it.
func TestReleaseSprintManagerLeaseStandsTheSeatDown(t *testing.T) {
	lease := seedSprintLease(t, "codex")
	if err := HoldSprintManagerLease(98, "codex", 4242); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseSprintManagerLease(98, "codex"); err != nil {
		t.Fatal(err)
	}
	got := lease()
	if !got.At.IsZero() || got.AttachedPID != 0 {
		t.Fatalf("released lease = %#v, want no heartbeat and no holding process", got)
	}
	if got.Holder != "codex" {
		t.Fatalf("Holder = %q, want it kept: erasing it hands a successor a blank record", got.Holder)
	}
}

// A watch exits BECAUSE somebody took the seat. If release were not
// holder-checked, that exit would stand the NEW occupant down on its way out —
// the losing process evicting the winner.
func TestReleaseSprintManagerLeaseRefusesAnotherHoldersSeat(t *testing.T) {
	lease := seedSprintLease(t, "successor")
	if err := ReleaseSprintManagerLease(98, "codex"); err == nil {
		t.Fatal("released a seat held by somebody else")
	}
	if lease().At.IsZero() {
		t.Fatal("the successor's heartbeat was cleared by the departing watch")
	}
}

// `sprint board` and `sprint take` grade the seat through the same helper the
// roster does, so a killed watch must free the seat here too — otherwise a
// successor waits out a full TTL behind a conductor that is already gone.
func TestSeatFreesUpWhenTheAttachedProcessIsGone(t *testing.T) {
	fresh := time.Now().UTC().Add(-time.Second)

	story := &weaveStory{ID: 98, Lease: &weaveStoryLease{Holder: "codex", At: fresh}}
	if _, stale, _ := weaveStoryLeaseState(story); stale {
		t.Fatal("a one-second-old heartbeat with no attached process read as stale")
	}

	story.Lease.AttachedPID = os.Getpid()
	if _, stale, _ := weaveStoryLeaseState(story); stale {
		t.Fatal("a live attached watch lost its own seat")
	}

	story.Lease.AttachedPID = retiredPID(t)
	holder, stale, free := weaveStoryLeaseState(story)
	if !stale {
		t.Fatal("a fresh beat from a dead watch still held the seat; a successor would wait out the TTL")
	}
	if free || holder != "codex" {
		t.Fatalf("state = (%q, stale=%v, free=%v), want the holder kept on a stale — not a vacant — seat",
			holder, stale, free)
	}
}

// retiredPID returns a pid that has certainly exited. Guessing a large number
// instead is how this kind of test flakes on a busy host.
func retiredPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process to retire: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process: %v", err)
	}
	if room.PidAlive(pid) {
		t.Skipf("pid %d was recycled before the assertion could use it", pid)
	}
	return pid
}
