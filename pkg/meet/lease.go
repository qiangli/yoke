package meet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// A meeting has ONE floor, so it may have only one runner.
//
// The append-only files made every individual write safe, and that was mistaken
// for the whole problem being solved. It is not: `st.Round++`, the turn loop,
// and the transcript append are a read-modify-write over shared state, and two
// processes running rounds against the same meeting interleave them. The
// 2026-07-18 artifact shows exactly that — a round-2 speaker still holding the
// floor when a round-3 speaker took it 22.6 seconds later, and events from
// different rounds interleaved afterwards. Nothing was corrupt line-by-line and
// the meeting was incoherent anyway.
//
// So round execution takes a lease. It is per-MEETING (the lock lives in the
// meeting's own directory), so independent meetings never contend, and a second
// runner is REFUSED with a clear message naming the owner rather than allowed
// to interleave.
//
// The lease is a KERNEL-HELD advisory lock (flock on unix, LockFileEx on
// Windows) on a stable run.lock inode, and that choice is the whole design:
//
//   - Ownership is decided by the kernel on a file DESCRIPTOR, in one atomic
//     operation. There is no read-then-act window, so there is nothing to race.
//     The earlier heartbeat/PID/mtime scheme could not avoid one: reading a
//     lock, judging it stale, and then removing it are three steps, and the file
//     at that path may be a different file by the third — no token comparison
//     fixes that, because compare-and-remove is not atomic over a path.
//   - Death releases it. When the owning process exits or its descriptor closes
//     for any reason, the kernel drops the lock. That removes the entire notion
//     of a stale lease: no heartbeat, no PID liveness probe, no stale window, no
//     break-and-recreate. A crashed meeting is runnable again immediately.
//   - The inode is stable. run.lock is created once and never unlinked, so two
//     processes that open the path are always contending on the SAME inode.
//     Deleting a lock file is what lets a successor lock a detached inode while
//     a predecessor still believes it owns the path.
//
// The metadata inside run.lock is therefore purely diagnostic — it exists so an
// operator can see who holds a busy meeting. A contender never parses it to
// decide ownership; the kernel already answered that.

// ErrMeetingBusy is returned when another process holds the meeting's run lease.
var ErrMeetingBusy = errors.New("meet: meeting is already being run by another process")

// leaseInfo is what a lock file says about its owner. It is written for a human
// reading the directory, never for a contender's ownership decision.
type leaseInfo struct {
	PID   int       `json:"pid"`
	Host  string    `json:"host"`
	Since time.Time `json:"since"`
}

// runLease is a held lease: the open descriptor IS the lease. Release is
// idempotent.
type runLease struct {
	lock *lockfile.Lock
	path string
}

func leasePath(id string) (string, error) {
	dir, err := storeDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "run.lock"), nil
}

// acquireRunLease takes the meeting's run lease, or reports that it is busy.
//
// Opening run.lock is unconditional — every contender opens the same stable
// inode — and the exclusive non-blocking lock on the resulting descriptor is
// what picks the single winner. Losers report ErrMeetingBusy and touch nothing.
func acquireRunLease(id string) (*runLease, error) {
	path, err := leasePath(id)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	l, err := lockfile.TryAcquire(path, lockfile.Holder{
		Name: host, PID: os.Getpid(), Intent: "run meeting", Since: nowFn(),
	})
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			return nil, busyError(err)
		}
		return nil, fmt.Errorf("meet: locking %s: %w", path, err)
	}

	return &runLease{lock: l, path: path}, nil
}

// Release drops the lease. Safe to call more than once, so callers can defer it
// unconditionally.
//
// It closes only ITS OWN descriptor, which is the whole reason a released owner
// cannot disturb its successor: the close affects one descriptor's lock and
// nothing at the path. run.lock itself is never removed.
func (l *runLease) Release() {
	if l == nil {
		return
	}
	_ = l.lock.Release()
}

// busyError names the current owner when it can, and stays useful when it
// cannot. The metadata is best-effort by construction: it may be empty (a lock
// taken microseconds ago, before its owner wrote), or left over from a previous
// owner. Neither affects who holds the lease — the kernel decided that — so an
// unreadable file degrades the message and nothing else.
type meetingBusyError struct {
	message string
	cause   error
}

func (e meetingBusyError) Error() string   { return e.message }
func (e meetingBusyError) Unwrap() []error { return []error{ErrMeetingBusy, e.cause} }

func busyError(err error) error {
	base := meetingBusyError{
		message: ErrMeetingBusy.Error() + "; wait for it to finish",
		cause:   err,
	}
	info, ok := lockfile.HeldBy(err)
	if !ok || info.PID <= 0 {
		return base
	}
	return meetingBusyError{
		message: fmt.Sprintf("%s (pid %d on %s since %s); wait for it to finish",
			ErrMeetingBusy, info.PID, info.Name, info.Since.Format(time.RFC3339)),
		cause: err,
	}
}
