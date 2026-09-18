//go:build windows

package weave

import (
	"fmt"

	"github.com/spf13/cobra"
)

func weaveHeartbeatDaemonize(cmd *cobra.Command, opts weaveHeartbeatOptions, queueDir, logFile string, flags *weaveOutputFlags) (bool, error) {
	// Background daemon mode is unsupported on Windows for the heartbeat command.
	// We fall back to running in the foreground so the user/agent can still use it.
	fmt.Fprintf(cmd.ErrOrStderr(), "heartbeat: background daemon mode is not supported on Windows; running in foreground\n")
	return false, nil
}
