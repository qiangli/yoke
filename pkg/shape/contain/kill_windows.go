//go:build windows

package contain

import "os"

func kill(pid int) { killPID(pid) }
func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
