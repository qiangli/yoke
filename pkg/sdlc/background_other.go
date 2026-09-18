//go:build !unix

package sdlc

import (
	"os"
	"os/exec"
)

func applyBackgroundProcAttrs(cmd *exec.Cmd) {}

// signalStop terminates the process (no process groups on non-unix).
func signalStop(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// processAlive reports whether pid is a live process. On Windows os.FindProcess
// opens the process handle via OpenProcess and fails for a nonexistent pid, so
// success (a usable handle) means the process is alive. (signal 0 isn't
// supported on Windows, so the unix probe can't be reused.)
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}
