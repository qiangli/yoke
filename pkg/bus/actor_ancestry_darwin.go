//go:build darwin

package bus

import "golang.org/x/sys/unix"

func parentProcessID(pid int) (int, bool) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || proc == nil {
		return 0, false
	}
	return int(proc.Eproc.Ppid), true
}
