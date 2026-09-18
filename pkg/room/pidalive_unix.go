//go:build !windows

package room

import (
	"os"
	"syscall"
)

// PidAlive reports whether a process exists. On Unix, signal 0 probes for the
// process without delivering anything (the classic liveness check).
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
