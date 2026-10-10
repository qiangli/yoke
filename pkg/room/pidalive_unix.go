//go:build !windows

package room

import (
	"os"
	"syscall"
)

// PidAlive reports whether pid names a process that is still RUNNING.
//
// Signal 0 asks only whether a pid EXISTS, and on Unix a process that has
// exited but not been reaped — a zombie — still answers it. That distinction
// was the `room "NAME" is already live (pid N)` loop: an inbox watcher died,
// its unreaped corpse kept answering the probe, and every new watch was
// refused by a holder that could never run again. Existence is therefore
// necessary but not sufficient: where the OS states whether the process has
// exited (processZombie), an exited holder is dead no matter what signal 0
// reports. Windows answers the same question its own way (GetExitCodeProcess,
// see pidalive_windows.go); a Unix that offers no state keeps the signal-0
// answer rather than guessing.
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if !signalZeroOK(pid) {
		return false
	}
	return !processZombie(pid)
}

// signalZeroOK is the classic existence probe: deliver nothing, ask only
// whether the kernel knows the pid.
func signalZeroOK(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
