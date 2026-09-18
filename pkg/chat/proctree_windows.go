//go:build windows

package chat

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Windows has no process groups in the POSIX sense and no kill(-pgid). The
// closest equivalent that costs nothing is CREATE_NEW_PROCESS_GROUP, which at
// least detaches the child from the parent's console-control events so a Ctrl-C
// aimed at bashy does not take the agent with it. Descendant cleanup on Windows
// would need a job object; the agent CLIs this drives are macOS/Linux-first, so
// the degraded behaviour here is a single-process kill — the same thing the
// runner did everywhere before this change.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func processTreeGone(err error) bool { return errors.Is(err, os.ErrProcessDone) }

func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func budgetOwnedGroupGone(cmd *exec.Cmd) bool { return cmd == nil || cmd.Process == nil }
