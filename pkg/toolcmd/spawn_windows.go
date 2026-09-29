//go:build windows

package toolcmd

import (
	"os"
	"syscall"
)

const detachedProcess = 0x00000008

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminatePID kills the runner: Windows has no SIGTERM for a detached
// process. CancelJob then records the job cancelled after its grace.
func terminatePID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
