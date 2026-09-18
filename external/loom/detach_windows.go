//go:build windows

package loom

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// setDetached starts the loom (Gitea) daemon in a new process group so it does
// not receive the parent console's Ctrl-C / CTRL_BREAK — the Windows
// counterpart of Setpgid in detach_unix.go.
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
