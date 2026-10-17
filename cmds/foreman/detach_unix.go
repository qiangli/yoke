//go:build unix

package foremancmd

import (
	"os/exec"
	"syscall"
)

func foremanDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func startForemanDetached(c *exec.Cmd) error {
	return c.Start()
}
