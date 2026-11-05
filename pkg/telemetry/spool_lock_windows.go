//go:build windows

package telemetry

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// A sentinel byte far beyond EOF coordinates processes without mandatory
// Windows range locking preventing a separate compaction/read handle from
// accessing the JSONL bytes. No rename or replacement of the shared spool is
// needed — the failure this file exists to avoid is the sharing-violation
// rename loop that produced the terabyte of abandoned scratch files.
func trySpoolLock(f *os.File) (bool, func() error, error) {
	ol := &windows.Overlapped{OffsetHigh: 0x40000000}
	h := windows.Handle(f.Fd())
	err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, func() error { return windows.UnlockFileEx(h, 0, 1, 0, ol) }, nil
}
