//go:build darwin

package bus

import (
	"bytes"

	"golang.org/x/sys/unix"
)

func parentProcessID(pid int) (int, bool) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || proc == nil {
		return 0, false
	}
	return int(proc.Eproc.Ppid), true
}

func processName(pid int) (string, bool) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || proc == nil {
		return "", false
	}
	comm := proc.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return string(comm), len(comm) > 0
}
