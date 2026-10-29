//go:build windows

package probe

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func browserDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// Some Windows launchers place bashy in a job that forbids breakaway. Retry
// without only that flag: detached/new-process-group semantics still keep the
// browser independent when the parent job permits child survival.
func startBrowserDetached(cmd *exec.Cmd) error {
	err := cmd.Start()
	if err == nil {
		return nil
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB != 0 && isAccessDenied(err) {
		cmd.SysProcAttr.CreationFlags &^= windows.CREATE_BREAKAWAY_FROM_JOB
		return cmd.Start()
	}
	return err
}

func isAccessDenied(err error) bool {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, os.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.ERROR_ACCESS_DENIED
}
