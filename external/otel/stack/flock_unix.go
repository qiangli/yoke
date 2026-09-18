//go:build !windows

package stack

import (
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// cleanStaleFlock removes flock.lock if the holding process is no longer alive.
// VictoriaMetrics calls os.Exit on lock failure, so we must clean up before Init.
func cleanStaleFlock(dataDir string) {
	lockPath := filepath.Join(dataDir, "flock.lock")
	f, err := os.Open(lockPath)
	if err != nil {
		return // no lock file — nothing to clean
	}
	defer f.Close()

	// Try to acquire an exclusive lock (non-blocking).
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		// We got the lock — no other process holds it. The lock is stale.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		if removeErr := os.Remove(lockPath); removeErr == nil {
			slog.Info("victoria-logs: removed stale lock file", "path", lockPath)
		}
		return
	}
	// Lock is held by a live process — leave it alone.
	slog.Warn("victoria-logs: lock file held by another process", "path", lockPath)
}
