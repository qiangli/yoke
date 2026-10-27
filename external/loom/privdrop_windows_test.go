//go:build windows

package loom

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

// Windows never drops (privdrop_windows.go), so a credential is never set.
func credOf(*exec.Cmd) (uid, gid int, ok bool) { return 0, 0, false }

func detachedOf(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP != 0
}
