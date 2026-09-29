package toolcmd

import (
	"context"
	"fmt"

	"github.com/qiangli/yoke/pkg/fleet"
)

// runTUI is a placeholder until S3 (toolcmd-tui) replaces this file.
func runTUI(ctx context.Context, tool fleet.Tool, cmd fleet.ToolCommand, args string, opts Options) (Result, error) {
	err := fmt.Errorf("toolcmd: %s:%s: tui mode not implemented yet", tool.Name, cmd.Name)
	return Result{Tool: tool.Name, Command: cmd.Name, Mode: cmd.Mode, Outcome: OutcomeError, Error: err.Error()}, err
}
