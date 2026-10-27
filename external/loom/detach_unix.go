//go:build unix

package loom

import (
	"os/exec"
	"syscall"
)

// setDetached puts the loom (Gitea) daemon in its own process group so it is
// not signalled together with the parent shell. Windows counterpart:
// detach_windows.go (CREATE_NEW_PROCESS_GROUP).
//
// It MERGES rather than assigns: privDrop.apply sets Credential on the same
// struct, and whichever ran first must not be erased by the other.
func setDetached(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
