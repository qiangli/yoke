//go:build !windows

package dag

import (
	"os/exec"
	"syscall"
)

func prepareCapacityProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
func capacityProcessGone(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return true
	}
	return cmd.ProcessState != nil && syscall.Kill(-cmd.Process.Pid, 0) == syscall.ESRCH
}

func capacityGroupGone(pid int) bool { return pid > 0 && syscall.Kill(-pid, 0) == syscall.ESRCH }
