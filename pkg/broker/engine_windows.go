//go:build windows

package broker

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func engineSysProcAttr() *syscall.SysProcAttr {
	// The engine runs in a job object that dies with its last handle: the
	// door holds the only one (engineJobHandle), so closing it in
	// stopEngineProcess stops the engine and every runner it spawned.
	// CREATE_NEW_PROCESS_GROUP keeps the engine out of the door's Ctrl-C
	// group, as Setpgid does on Unix.
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// detachSysProcAttr requests breakaway from the launcher's job object so
// the door process outlives a parent timeout job, while keeping detached
// process and new process group semantics.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// startDetached launches cmd, falling back to non-breakaway spawn if the
// parent job forbids breakaway (e.g. nested in an SSH session job).
func startDetached(c *exec.Cmd) error {
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
	if cmd.SysProcAttr != nil && (cmd.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB) != 0 && isAccessDenied(err) {
		cmd.SysProcAttr.CreationFlags &^= windows.CREATE_BREAKAWAY_FROM_JOB
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
