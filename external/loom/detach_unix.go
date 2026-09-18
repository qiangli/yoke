//go:build unix

package loom

import (
	"os/exec"
	"syscall"
)

// setDetached puts the loom (Gitea) daemon in its own process group so it is
// not signalled together with the parent shell. Windows counterpart:
// detach_windows.go (CREATE_NEW_PROCESS_GROUP).
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
