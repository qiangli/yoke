//go:build linux

package runner_embed

import (
	"os/exec"
	"syscall"
)

// setDeathSignal kills the CUDA runner with its supervisor: the ollama
// server stops a runner with SIGKILL, which the supervisor cannot forward.
func setDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
