//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package telemetry

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// The spool inode is stable: rotation must never rename it. The kernel releases
// this lock if a writer exits, including an ungraceful process termination —
// which is why the lock lives on the file itself rather than in a lock file
// that a killed process would leave behind.
func trySpoolLock(f *os.File) (bool, func() error, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, func() error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }, nil
}
