package weave

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCooldownContentionNamesTheCooldownLock pins what the operator is told
// when cooldown.lock — not queue.lock — is the contended lock.
//
// This path is NEW. Before the shared-wrapper refactor, cooldown.lock was taken
// with a plain blocking syscall.Flock(LOCK_EX) on unix and was a no-op on
// Windows, so a cooldown write could never report contention on either
// platform. Routing it through weaveFlock made it bounded — an improvement —
// and simultaneously made errWeaveQueueBusy reachable from a lock that is
// deliberately NOT the queue lock. errWeaveQueueBusy's text is a sentence about
// the queue: "another weave command holds the queue lock; retry".
//
// The whole justification for a separate cooldown.lock, stated in
// weave_cooldown.go and repeated in withWeaveCooldownLock's own doc comment, is
// that a best-effort cooldown write must never contend with a queue state
// transition. So the one message emitted when cooldown.lock is contended
// asserts the exact condition the design guarantees is false — and it is
// falsifiable on the spot: queue.lock is demonstrably free while it is printed.
//
// The message is not decoration. weave_impl.go prints it verbatim to stderr
// ("weave start: cooldown record failed (continuing): %v"), which sends the
// operator to inspect queue.lock, `weave list` and other weave processes — all
// of which will look healthy, because they are.
func TestCooldownContentionNamesTheCooldownLock(t *testing.T) {
	dir := t.TempDir()

	prevWait := weaveQueueLockWait
	weaveQueueLockWait = 100 * time.Millisecond
	t.Cleanup(func() { weaveQueueLockWait = prevWait })

	// Hold cooldown.lock, and ONLY cooldown.lock.
	release, err := weaveFlock(filepath.Join(dir, "cooldown.lock"), time.Second)
	if err != nil {
		t.Fatalf("take cooldown.lock: %v", err)
	}
	defer release()

	err = recordToolCooldown(dir, "codex", time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("recording a cooldown succeeded while cooldown.lock was held")
	}

	// The premise of the message, checked rather than assumed: the queue lock
	// is free. Anyone acting on this error is being sent somewhere empty.
	queueRelease, qerr := weaveFlock(filepath.Join(dir, "queue.lock"), 0)
	if qerr != nil {
		t.Fatalf("queue.lock was genuinely held — this test's premise is wrong: %v", qerr)
	}
	queueRelease()

	if strings.Contains(err.Error(), "queue lock") {
		t.Errorf("cooldown contention blames the queue lock, which is free:\n  got: %v", err)
	}
	if !strings.Contains(err.Error(), "cooldown.lock") {
		t.Errorf("cooldown contention does not name the lock that is actually held (cooldown.lock):\n  got: %v", err)
	}
	if errors.Is(err, errWeavePullBusy) {
		t.Errorf("cooldown contention must not impersonate pull contention: %v", err)
	}
}
