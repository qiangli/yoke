//go:build !unix

package meet

import (
	"os"
	"os/exec"
)

// The non-unix half of the daemon lifecycle (see service.go). Windows has no
// process groups to detach into and no SIGTERM to ask politely with, so the
// graceful and forceful stops collapse into the same call — TerminateProcess.
// The grace loop in StopService still runs; it simply finds the process already
// gone on the first poll.

func applyBackgroundProcAttrs(cmd *exec.Cmd) {}

func signalStop(pid int) error { return terminate(pid) }

func forceStop(pid int) error { return terminate(pid) }

func terminate(pid int) error {
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
// success (a usable handle) means the process is alive. (Signal 0 isn't
// supported there, so the unix probe can't be reused.)
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}
