//go:build darwin

package room

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// darwinStateZombie is p_stat SZOMB from <sys/proc.h>: the process has
// exited and only its un-reaped entry remains. x/sys does not export the S*
// states, so the one this file judges on is pinned here.
const darwinStateZombie = 5

// processZombie asks the kernel directly. kern.proc.pid answers for any
// process on the host, exited or not, which is exactly the question
// signal 0 cannot separate.
func processZombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// No kinfo, no verdict: keep the signal-0 answer already made.
		return false
	}
	return kp.Proc.P_stat == darwinStateZombie
}

// pidStart is the kernel's start time for pid — a value set once at exec and
// never rewritten — as the same-process half of a holder's identity. The
// encoding is opaque on purpose: it is only ever compared against another
// answer to this same question, never interpreted.
func pidStart(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", false
	}
	tv := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), true
}
