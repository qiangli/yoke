//go:build windows

package broker

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// engineJobHandle is the job object the door starts its engine in. Zero when
// no engine is running (or it is already stopped). The door holds the only
// inheritable handle, so closing it in stopEngineProcess kills the engine
// tree (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) — the one way a parent on
// Windows takes a whole subtree down, since Kill is per-process.
var engineJobHandle windows.Handle

// putEngineInJob moves a just-started engine into that job object and records
// the handle.
func putEngineInJob(pid int) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return err
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(proc)
	if err = windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return err
	}
	engineJobHandle = job
	return nil
}

// closeEngineJob closes that handle, stopping the engine tree; it reports
// whether a job was open.
func closeEngineJob() (bool, error) {
	if engineJobHandle == 0 {
		return false, nil
	}
	err := windows.CloseHandle(engineJobHandle)
	engineJobHandle = 0
	return true, err
}

// ExecEngine.Start's Windows half: put the engine in the kill-on-close job
// object above. (engine.go declares engineJobHook nil for the other ports.)
func init() { engineJobHook = func(cmd *exec.Cmd) error { return putEngineInJob(cmd.Process.Pid) } }
