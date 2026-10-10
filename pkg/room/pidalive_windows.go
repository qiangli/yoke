//go:build windows

package room

import (
	"strconv"
	"syscall"
)

// Windows access rights / exit-code sentinels not exported by the std syscall
// package. PROCESS_QUERY_LIMITED_INFORMATION is the least-privileged right that
// still permits a liveness/exit-code query (Vista+); STILL_ACTIVE (259) is the
// GetExitCodeProcess code returned while a process is still running.
const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// PidAlive reports whether a process exists. On Windows the Unix "signal 0"
// probe is unavailable — (*os.Process).Signal rejects every signal but Kill —
// so we ask the kernel directly: open a query handle (a fully-gone pid fails
// OpenProcess) and confirm the exit code is STILL_ACTIVE (an already-exited but
// still-referenced process reports its real exit code instead).
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		// Handle opened but the code query failed — treat the presence of a
		// live handle as alive rather than falsely pruning it.
		return true
	}
	return code == stillActive
}

// pidStart is the process creation time, as the same-process half of a
// holder's identity: a pid is only the recorded holder while the process
// that was created at the recorded time still wears it. The encoding is
// opaque on purpose — it is only ever compared against another answer to
// this same question, never interpreted.
func pidStart(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer syscall.CloseHandle(h)
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", false
	}
	return strconv.FormatUint(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime), 10), true
}
