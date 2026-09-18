//go:build windows

package engine

import "fmt"

// killProcess stub for Windows. The proper implementation uses
// TerminateProcess from golang.org/x/sys/windows; ycode podman isn't
// supported on Windows yet so log-and-skip rather than gate.
func killProcess(pid int) error {
	return fmt.Errorf("host cleanup kill not implemented on windows: pid %d", pid)
}
