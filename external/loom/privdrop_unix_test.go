//go:build unix

package loom

import (
	"os/exec"
	"syscall"
)

// credOf reports the uid/gid the command would start under, and whether a
// credential was set at all (the whole point of the drop).
func credOf(cmd *exec.Cmd) (uid, gid int, ok bool) {
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		return 0, 0, false
	}
	c := cmd.SysProcAttr.Credential
	return int(c.Uid), int(c.Gid), true
}

func detachedOf(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
}

var _ = syscall.Credential{}
