//go:build unix

package engine

import "syscall"

// killProcess sends SIGKILL to pid. Used by host cleanup to terminate
// orphaned vfkit/gvproxy processes. SIGTERM is the polite first
// attempt but vfkit ignores it (we saw this today during the broken-
// machine incident), so we go straight to SIGKILL.
func killProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
