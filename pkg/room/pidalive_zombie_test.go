//go:build !windows

package room

// THE DEAD-WATCHER LOOP (story 62a6c1e6). An `bashy inbox --as NAME --watch`
// process died, and every new watch was refused with
// `room "NAME" is already live (pid N)` although pid N no longer existed.
//
// The corpse is the mechanism: on Unix a process that has exited but not
// been reaped — a zombie — still answers signal 0, so an existence probe
// vouches for a holder that cannot ever run again. Each test here fails
// against a liveness check that asks only "does the pid exist".

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// zombiePID returns a pid that has EXITED but has not been reaped: the shape
// a dead watcher leaves behind when its parent never waits on it. The parent
// here is this test process, which deliberately does not wait until cleanup.
func zombiePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child to abandon: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() }) // reap on the way out; do not leak it
	pid := cmd.Process.Pid
	for i := 0; i < 100 && PidAlive(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	return pid
}

// liveChildPID returns a pid that is running and is not this process, so a
// test can play two distinct live members without adopting the harness's pid.
func liveChildPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a live child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// A liveness check must not vouch for a corpse. Signal 0 answers for a
// zombie; the room must not, or a dead holder fences its id forever.
func TestPidAliveDoesNotVouchForAZombie(t *testing.T) {
	pid := zombiePID(t)
	if PidAlive(pid) {
		t.Fatalf("pid %d has exited and is unreaped, yet reports live — the probe answers for a corpse", pid)
	}
}

// THE INCIDENT: a watcher card whose holder has exited must be STALE, so the
// next `inbox --as NAME --watch` takes the seat instead of being refused by
// the dead holder in a loop.
func TestJoinTakesOverADeadWatchersCard(t *testing.T) {
	isolate(t)
	id := AgentClaimID("omar")
	dead := zombiePID(t)
	// The card an inbox watcher publishes: its own pid as writer, its
	// harness as owner. holderPID judges it on the writer (no session claim).
	if err := Join(Card{ID: id, Nick: "omar", Tool: "codex",
		Binding: "codex:opus5", Mode: "inbox", PID: dead, OwnerPID: os.Getpid()}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A new watch must take the seat, not be fenced out by the corpse.
	if err := Join(Card{ID: id, Nick: "omar", Tool: "codex",
		Binding: "codex:opus5", Mode: "inbox", PID: os.Getpid()}); err != nil {
		t.Fatalf("a dead holder (pid %d) fenced a new watch: %v", dead, err)
	}
	got, ok, _ := Find(id)
	if !ok || got.PID != os.Getpid() {
		t.Fatalf("card = %+v (%v), want the new watcher holding the seat", got, ok)
	}
}

// Reading is the reconciliation: a zombie-held card must not appear in the
// live membership either, or the board asserts a dead member is live.
func TestMembersPrunesAZombieHolder(t *testing.T) {
	isolate(t)
	id := AgentClaimID("ghost")
	if err := Join(Card{ID: id, Nick: "ghost", Tool: "codex",
		Binding: "codex:opus5", Mode: "inbox", PID: zombiePID(t)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, ok, _ := Find(id); ok {
		t.Fatal("a zombie holder is still on the board")
	}
}

// A recorded holder pid that now belongs to a DIFFERENT process — a recycled
// pid — is not the holder. The card is seeded as raw JSON with this test
// process as the holder and a start time that is not this process's, which
// is exactly what a reclaimed pid looks like to a card written before its
// holder died.
func TestACardWhosePidWasRecycledIsStale(t *testing.T) {
	isolate(t)
	id := AgentClaimID("recycled")
	var raw map[string]any
	b, err := json.Marshal(Card{ID: id, Nick: "recycled", Tool: "codex",
		Binding: "codex:opus5", Mode: "inbox", PID: os.Getpid(), Joined: now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw["holder_start"] = "1.000000" // a start time this pid never had
	dir, err := membersDir()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memberPath(dir, id), seed, 0o600); err != nil {
		t.Fatal(err)
	}

	// A live newcomer must take over rather than be fenced by a stranger
	// that happens to wear the old holder's pid.
	newcomer := liveChildPID(t)
	if err := Join(Card{ID: id, Nick: "recycled", Tool: "codex",
		Binding: "codex:opus5", Mode: "inbox", PID: newcomer}); err != nil {
		var live *ErrLive
		if errors.As(err, &live) {
			t.Fatalf("holder pid %d was recycled (start time mismatch), yet still fenced the seat: %v", os.Getpid(), err)
		}
		t.Fatalf("takeover: %v", err)
	}
	got, ok, _ := Find(id)
	if !ok || got.PID != newcomer {
		t.Fatalf("card = %+v (%v), want the newcomer holding the seat", got, ok)
	}
}
