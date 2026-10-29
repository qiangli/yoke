//go:build !windows

package contain

import "syscall"

func kill(pid int)    { _ = syscall.Kill(-pid, syscall.SIGKILL) }
func killPID(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
