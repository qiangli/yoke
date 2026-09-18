//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package gate

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// prepareGateCommand makes cancellation reach the gate's descendants too.
// exec.CommandContext otherwise kills only `sh`, leaving test/build children
// orphaned and still consuming resources after a sprint timeout.
func prepareGateCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
