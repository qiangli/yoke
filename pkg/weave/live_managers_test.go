package weave

import (
	"os"
	"testing"
	"time"
)

// The whole value of --role conductor is that it names agents that will
// actually READ the message. An unowned sprint names nobody, and a stale lease
// names a conductor that died without handing off — addressing either produces
// mail no one answers, which is worse than a broadcast because the sender
// believes it landed.
func TestLiveSprintManagersExcludesStaleAndUnowned(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", dir)

	now := time.Now().UTC()
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Stories = append(q.Stories,
			&weaveStory{ID: 1, Title: "live", Column: "doing",
				Lease: &weaveStoryLease{Holder: "zoe", At: now.Add(-time.Minute)}},
			&weaveStory{ID: 2, Title: "stale", Column: "doing",
				Lease: &weaveStoryLease{Holder: "ghost", At: now.Add(-SprintLeaseTTL - time.Minute)}},
			&weaveStory{ID: 3, Title: "unowned", Column: "backlog"},
			&weaveStory{ID: 4, Title: "empty holder", Column: "doing",
				Lease: &weaveStoryLease{Holder: "   ", At: now}},
			// One agent may manage several sprints; it is one addressee.
			&weaveStory{ID: 5, Title: "second seat", Column: "doing",
				Lease: &weaveStoryLease{Holder: "zoe", At: now}},
			&weaveStory{ID: 6, Title: "also live", Column: "doing",
				Lease: &weaveStoryLease{Holder: "amos", At: now}},
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LiveSprintManagers()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"amos", "zoe"} // sorted, deduplicated
	if len(got) != len(want) {
		t.Fatalf("LiveSprintManagers() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LiveSprintManagers() = %v, want %v", got, want)
		}
	}
}

// A lease exactly at the TTL boundary is graded the same way the board and
// `bashy agent` grade it — that is why SprintLeaseTTL is exported.
func TestLiveSprintManagersUsesTheSharedLeaseTTL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", dir)

	now := time.Now().UTC()
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Stories = append(q.Stories,
			&weaveStory{ID: 1, Title: "just inside", Column: "doing",
				Lease: &weaveStoryLease{Holder: "inside", At: now.Add(-SprintLeaseTTL + time.Minute)}},
			&weaveStory{ID: 2, Title: "just outside", Column: "doing",
				Lease: &weaveStoryLease{Holder: "outside", At: now.Add(-SprintLeaseTTL - time.Minute)}},
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LiveSprintManagers()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "inside" {
		t.Fatalf("LiveSprintManagers() = %v, want [inside] — the TTL must match the board's", got)
	}
}

// A selector must make the same liveness decision as the shared sprint seat:
// an attached watch that has exited does not remain a manager for its TTL,
// while a fresh ephemeral heartbeat and a live attached watch both do.
func TestLiveSprintManagersUsesSharedSeatLiveness(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", dir)

	now := time.Now().UTC()
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Stories = append(q.Stories,
			&weaveStory{ID: 1, Title: "ephemeral", Column: "doing",
				Lease: &weaveStoryLease{Holder: "ephemeral", At: now}},
			&weaveStory{ID: 2, Title: "attached and live", Column: "doing",
				Lease: &weaveStoryLease{Holder: "watching", At: now, AttachedPID: os.Getpid()}},
			&weaveStory{ID: 3, Title: "attached and gone", Column: "doing",
				Lease: &weaveStoryLease{Holder: "ghost", At: now, AttachedPID: retiredPID(t)}},
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LiveSprintManagers()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ephemeral", "watching"}
	if len(got) != len(want) {
		t.Fatalf("LiveSprintManagers() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LiveSprintManagers() = %v, want %v", got, want)
		}
	}
}

// An empty board is not an error: nobody is seated, so nobody is addressed.
func TestLiveSprintManagersOnAnEmptyBoard(t *testing.T) {
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	got, err := LiveSprintManagers()
	if err != nil {
		t.Fatalf("empty board must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LiveSprintManagers() = %v, want none", got)
	}
}
