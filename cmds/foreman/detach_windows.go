//go:build windows

package foremancmd

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	createBreakawayFromJob = 0x01000000
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
)

// foremanDetachedSysProcAttr requests breakaway from the launcher's job object
// so the supervisor outlives a parent timeout job, while keeping detached
// process and new process group semantics.
func foremanDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: createBreakawayFromJob | detachedProcess | createNewProcessGroup,
	}
}

// startForemanDetached launches cmd, falling back to non-breakaway spawn if
// the parent job forbids breakaway (e.g. nested in an SSH session job).
func startForemanDetached(c *exec.Cmd) error {
	return startWithBreakawayRetry(c, c.Start)
}

func startWithBreakawayRetry(cmd *exec.Cmd, start func() error) error {
	if start == nil {
		start = cmd.Start
	}
	err := start()
	if err == nil {
		return nil
	}
	if cmd.SysProcAttr != nil && (cmd.SysProcAttr.CreationFlags&createBreakawayFromJob) != 0 && isAccessDenied(err) {
		cmd.SysProcAttr.CreationFlags &^= createBreakawayFromJob
		return start()
	}
	return err
}

func isAccessDenied(err error) bool {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, os.ErrPermission) {
		return true
	}
	var sysErr syscall.Errno
	if errors.As(err, &sysErr) && sysErr == syscall.ERROR_ACCESS_DENIED {
		return true
	}
	var winErr windows.Errno
	if errors.As(err, &winErr) && winErr == windows.ERROR_ACCESS_DENIED {
		return true
	}
	return false
}
