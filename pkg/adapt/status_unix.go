//go:build !windows

package adapt

import (
	"os/exec"
	"syscall"
)

func signalStatus(e *exec.ExitError) int {
	if s, ok := e.Sys().(syscall.WaitStatus); ok && s.Signaled() {
		return 128 + int(s.Signal())
	}
	return 1
}
