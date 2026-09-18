//go:build windows

package weave

import "syscall"

// weaveMaybeSetsid is a no-op on Windows; we don't have setsid
// semantics there, and the PTY path doesn't work either, so a
// backgrounded `bashy weave start` on Windows already has limited
// guarantees vs. its launching console.
func weaveMaybeSetsid(parentStdinTTY bool) {}

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// pidAlive asks Windows whether pid still has the STILL_ACTIVE exit code.
// A handle that opens but cannot be queried is treated conservatively as alive
// rather than allowing a duplicate wrapper to be started.
func pidAlive(pid int) bool {
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
		return true
	}
	return code == stillActive
}

// weaveStopWrapper on Windows is unimplemented for the MVP — the
// rest of the weave PTY/setsid path is unix-only too. Adding job-
// object based termination here would let `weave abandon` work on
// Windows but is deferred until someone actually needs it.
func weaveStopWrapper(pid int) {}
