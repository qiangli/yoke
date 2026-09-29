//go:build !windows

package toolcmd

import (
	"os"
	"syscall"
)

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// terminatePID sends SIGTERM; the runner cancels its context and records
// the job cancelled.
func terminatePID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}
