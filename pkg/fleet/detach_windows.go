//go:build windows

package fleet

import (
	"errors"
	"os/exec"
	"syscall"
)

// createBreakawayFromJob and detachedProcess are Win32 CreateProcess flags
// not exposed by the stdlib syscall package on windows (unlike
// CREATE_NEW_PROCESS_GROUP).
const (
	createBreakawayFromJob = 0x01000000
	detachedProcess        = 0x00000008
)

// detachSysProcAttr breaks bashy supervisord out of any job object wrapping
// this process — a CI harness's timeout job, a shell's job under Windows
// Terminal — so supervisord outlives it instead of dying when the wrapping
// job is torn down.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: createBreakawayFromJob | detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

// startDetached starts c, retrying once without CREATE_BREAKAWAY_FROM_JOB
// if the job wrapping this process forbids escaping it: a job created
// without JOB_OBJECT_LIMIT_BREAKAWAY_OK/SILENT_BREAKAWAY_OK makes
// CreateProcess fail outright with ERROR_ACCESS_DENIED rather than ignore
// the flag, which would otherwise leave supervisord unstarted entirely.
func startDetached(c *exec.Cmd) error {
	return startDetachedRetry(c.Start, c.SysProcAttr)
}

// startDetachedRetry is startDetached's retry decision, factored out so a
// test can drive it with a fake start func instead of a real CreateProcess
// call.
func startDetachedRetry(start func() error, attr *syscall.SysProcAttr) error {
	err := start()
	if err == nil || attr == nil || attr.CreationFlags&createBreakawayFromJob == 0 || !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		return err
	}
	attr.CreationFlags &^= createBreakawayFromJob
	return start()
}
