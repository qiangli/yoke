//go:build !windows

package fleet

import (
	"os/exec"
	"syscall"
)

// detachSysProcAttr starts bashy supervisord in its own session, so it
// outlives the process that asked EnsureSupervisordRunning to start it.
func detachSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// startDetached starts c. Unix has no job-object concept to escape, so this
// is just c.Start(); see detach_windows.go for the breakaway retry.
func startDetached(c *exec.Cmd) error { return c.Start() }
