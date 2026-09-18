//go:build !windows

package weave

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/spf13/cobra"
)

func weaveHeartbeatDaemonize(cmd *cobra.Command, opts weaveHeartbeatOptions, queueDir, logFile string, flags *weaveOutputFlags) (bool, error) {
	mode := flags.mode()
	const op = "weave heartbeat"

	if os.Getenv("BASHY_HEARTBEAT_DAEMON") == "1" {
		return false, nil
	}

	self, err := os.Executable()
	if err != nil {
		return false, err
	}

	// Filter out --background or -background from arguments to avoid loop
	var args []string
	for _, arg := range os.Args[1:] {
		if arg == "--background" || arg == "-background" {
			continue
		}
		args = append(args, arg)
	}

	f, err := weaveAppendFile(logFile, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()

	childCmd := exec.Command(self, args...)
	childCmd.Env = append(os.Environ(), "BASHY_HEARTBEAT_DAEMON=1")
	childCmd.Stdout = f
	childCmd.Stderr = f
	childCmd.Stdin = nil
	childCmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	if err := childCmd.Start(); err != nil {
		return false, err
	}

	if mode == weavecli.OutputJSON {
		_ = emitOK(cmd.OutOrStdout(), mode, op, map[string]any{
			"pid":      childCmd.Process.Pid,
			"log_file": logFile,
		})
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "heartbeat daemon started (pid %d); logging to %s\n", childCmd.Process.Pid, logFile)
	}
	return true, nil
}
