//go:build unix

package loom

import (
	"io/fs"
	"os/exec"
	"syscall"
)

// apply is where the privilege actually drops: the child is started directly
// under the account's uid/gid, so Gitea never holds root even momentarily.
// Merges into any SysProcAttr already set (setDetached's Setpgid).
func (d *privDrop) apply(cmd *exec.Cmd) {
	if d == nil || cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(d.UID), Gid: uint32(d.GID)}
}

// fileOwner reports the uid/gid of a stat result, so own() can skip an entry
// that is already correct.
func fileOwner(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
