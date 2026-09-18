//go:build unix

package meet

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// The unix half of the daemon lifecycle (see service.go). Mirrors
// pkg/sdlc/background_unix.go, with one addition: signals go to the daemon's
// process GROUP only when the pid actually leads one, so a stale or recycled
// pidfile can never take down an unrelated group.

// applyBackgroundProcAttrs detaches the daemon into its own process group, so
// closing the terminal that started it does not take the server down with it,
// and so a stop can reach anything it spawned.
func applyBackgroundProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processAlive reports whether pid is a live process. On unix os.FindProcess
// always succeeds, so probe with signal 0 (EPERM still means it exists).
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalStop(pid int) error { return kill(pid, syscall.SIGTERM) }
func forceStop(pid int) error  { return kill(pid, syscall.SIGKILL) }
func leadsGroup(pid int) bool  { pgid, err := syscall.Getpgid(pid); return err == nil && pgid == pid }

func kill(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	// The group first (the daemon was started with Setpgid, so pgid == pid), but
	// only once that is confirmed: kill(-pid) against a pid that merely happens
	// to sit in somebody else's group would signal that whole group.
	if leadsGroup(pid) {
		_ = syscall.Kill(-pid, sig)
	}
	return syscall.Kill(pid, sig)
}
