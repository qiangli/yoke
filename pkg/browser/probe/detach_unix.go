//go:build !windows

package probe

import (
	"os/exec"
	"syscall"
)

func browserDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func startBrowserDetached(cmd *exec.Cmd) error { return cmd.Start() }
