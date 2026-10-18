//go:build !windows

package foreman

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// terminateGrace and terminateProcessStart are vars so tests can shorten the wait
// and recycle a pid deterministically.
var (
	terminateGrace        = 5 * time.Second
	terminateProcessStart = ProcessStart
)

// terminateReaped reports that pid has exited. A var so a test that is the
// fixture's parent can count its own reap as the exit.
var terminateReaped = func(pid int) bool { return !room.PidAlive(pid) }

// Terminate ends a session whose serve process no longer answers its control
// socket — the one case a CommandStop sent over that socket cannot reach.
//
// It acts only on PROOF: the session is not already stopped, the recorded serve
// pid is alive, is not the caller, and its kernel start identity still matches
// the one the serve process recorded for itself. Anything less is
// ErrServeNotProven and nothing is signalled, because a recycled pid belongs to
// somebody else. SIGTERM first, SIGKILL after a grace period; then the session
// is recorded as stopped and its socket removed so the next start is clean.
func Terminate(root, id, reason string) error {
	store := NewStore(root, id)
	st, err := store.LoadState()
	if err != nil {
		return err
	}
	if st.Stopped {
		return nil
	}
	pid := st.ServePID
	if pid <= 0 || pid == os.Getpid() || st.ServeStart == "" || !room.PidAlive(pid) {
		return fmt.Errorf("%w: session %s records serve pid %d", ErrServeNotProven, id, pid)
	}
	if !serveIs(pid, st.ServeStart) {
		return fmt.Errorf("%w: session %s pid %d is a different process now", ErrServeNotProven, id, pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("foreman: terminate session %s pid %d: %w", id, pid, err)
	}
	if !waitReaped(pid, terminateGrace) {
		// The grace period is long enough for the serve process to exit and its
		// pid to be recycled: prove it again before the signal that cannot be caught.
		if !serveIs(pid, st.ServeStart) {
			return fmt.Errorf("%w: session %s pid %d changed identity before SIGKILL", ErrServeNotProven, id, pid)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
		if !waitReaped(pid, terminateGrace) {
			return fmt.Errorf("foreman: session %s pid %d survived SIGKILL", id, pid)
		}
	}
	// Another serve process may have taken the session over while this waited;
	// its state and socket are its own and are left alone.
	cur, err := store.LoadState()
	if err != nil {
		return err
	}
	if cur.ServePID != pid || cur.ServeStart != st.ServeStart {
		return fmt.Errorf("%w: session %s was replaced while terminating pid %d", ErrServeNotProven, id, pid)
	}
	if !cur.Stopped {
		cur.Stopped = true
		cur.Status = StatusBlocked
		cur.StopReason = "terminated: " + reason
		cur.Steering = false
		if err := store.SaveState(cur); err != nil {
			return err
		}
	}
	_ = os.Remove(store.CtlSockPath())
	return nil
}

// serveIs reports that pid is still the process whose start identity is start.
func serveIs(pid int, start string) bool {
	got, ok := terminateProcessStart(pid)
	return ok && got == start
}

func waitReaped(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if terminateReaped(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}
