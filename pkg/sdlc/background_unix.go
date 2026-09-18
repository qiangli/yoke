//go:build unix

package sdlc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

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

// signalStop asks the process (and its group, since Serve is a group leader via
// Setpgid) to terminate.
func signalStop(pid int) error {
	if pid <= 0 {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	return syscall.Kill(pid, syscall.SIGTERM)
}
