//go:build windows

// Package procguard starts a command behind a kill-on-parent-exit guard.
package procguard

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Guard owns a Job Object created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
// The only handle to it lives in this process, so when the parent exits —
// cleanly or killed — the kernel closes the handle and terminates every
// process in the job: the command and all of its descendants. Arm must be
// called before starting cmd; Started must be called with the start result,
// and Disarm immediately after cmd has been reaped.
type Guard struct {
	mu  sync.Mutex
	job windows.Handle
	cmd *exec.Cmd
}

// ContainsSessionEscapes reports whether this backend contains a descendant
// that tries to leave the guarded tree. A Job Object does: membership is
// inherited by every child, and the job grants neither BREAKAWAY_OK nor
// SILENT_BREAKAWAY_OK, so CREATE_BREAKAWAY_FROM_JOB fails rather than escapes.
func ContainsSessionEscapes() bool { return true }

// Arm creates the kill-on-close job and arranges for cmd to start suspended,
// so Started can assign it to the job before it runs a single instruction —
// no grandchild can be spawned outside the job. Path, Args, Dir, Env and
// stdio are left exactly as the caller set them.
func Arm(cmd *exec.Cmd) (*Guard, error) {
	if cmd == nil || cmd.Path == "" || len(cmd.Args) == 0 {
		return nil, fmt.Errorf("procguard: command is not initialized")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("procguard: create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("procguard: configure job object: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	return &Guard{job: job, cmd: cmd}, nil
}

// Started completes Arm after cmd.Start: it assigns the suspended process to
// the job and only then resumes it. If assignment fails the process is
// terminated while still suspended — it never runs unguarded.
func (g *Guard) Started(startErr error) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if startErr != nil || g.cmd.Process == nil {
		g.closeLocked(false)
		return
	}
	pid := uint32(g.cmd.Process.Pid)
	if err := g.assign(pid); err != nil {
		_ = g.cmd.Process.Kill()
		g.closeLocked(false)
		return
	}
	if err := resumeProcess(pid); err != nil {
		// Assigned but unresumable: terminating the job is the fail-closed
		// outcome, and the caller's Wait reports it.
		_ = windows.TerminateJobObject(g.job, 127)
	}
}

func (g *Guard) assign(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(g.job, h)
}

// resumeProcess resumes every thread of a CREATE_SUSPENDED process. os/exec
// does not expose the primary-thread handle, so the threads are found by pid.
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			return oerr
		}
		_, rerr := windows.ResumeThread(th)
		_ = windows.CloseHandle(th)
		if rerr != nil {
			return rerr
		}
		resumed++
	}
	if resumed == 0 {
		return fmt.Errorf("procguard: no thread found for process %d", pid)
	}
	return nil
}

// Disarm releases the job after the guarded command has been reaped. Like the
// POSIX guard, it is a parent-death trigger, not a reaper: descendants the
// command deliberately left running after a normal exit are not killed here
// (the kill limit is cleared before the handle closes). While armed, the
// parent's death closes the handle and kills the whole tree.
func (g *Guard) Disarm() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeLocked(true)
}

func (g *Guard) closeLocked(release bool) {
	if g.job == 0 {
		return
	}
	if release {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		_, _ = windows.SetInformationJobObject(g.job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	}
	_ = windows.CloseHandle(g.job)
	g.job = 0
}
