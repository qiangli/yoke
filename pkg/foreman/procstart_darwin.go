package foreman

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcessStart is pid's kernel start time, which a recycled pid does not share.
func ProcessStart(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return "", false
	}
	t := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec), true
}
