// Package toolcmd runs a tool's declared vendor commands (fleet.ToolCommand)
// as bashy services: a thin runner over chat.Invoke (print mode) and
// chat.Session (tui mode). Everything tool-specific is YAML data; see
// docs/tool-commands-design.md (Sprint #324).
package toolcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// Outcomes reported in Result.Outcome.
const (
	OutcomeSuccess     = "success"
	OutcomeError       = "error"
	OutcomeUnavailable = "unavailable" // the tool refused the command in this mode (e.g. a TUI-only built-in in print mode)
	OutcomeTimeout     = "timeout"
	OutcomeCancelled   = "cancelled"
	OutcomeDryRun      = "dry-run"
)

// Output modes of fleet.ToolCommand.Output.
const (
	OutputTurn       = "turn"
	OutputTranscript = "transcript"
	OutputFilePrefix = "file:"
)

// DefaultTimeout bounds a command whose YAML declares none.
const DefaultTimeout = 30 * time.Minute

// Options tune one command run.
type Options struct {
	// Dir is the working directory. Empty = a fresh temp dir (kept when the
	// command collects file artifacts, removed otherwise).
	Dir string
	// Timeout overrides the command's declared timeout (0 = declared, else DefaultTimeout).
	Timeout time.Duration
	// Model is passed to the tool's {model} token / model flag.
	Model string
	// Agent overrides the agent nick used to resolve the launch (default: the tool name).
	Agent string
	// Stdout, when set, receives the tool's stdout as it is written (tee).
	Stdout io.Writer
	// Stderr, when set, receives diagnostics.
	Stderr io.Writer
	// DryRun renders the argv (print) or steps (tui) without running anything.
	DryRun bool
}

// Result is the structured envelope of one command run.
type Result struct {
	Tool      string    `json:"tool"`
	Command   string    `json:"command"`
	Mode      string    `json:"mode"`
	Slash     string    `json:"slash,omitempty"` // the rendered slash line
	Argv      []string  `json:"argv,omitempty"`  // print: the rendered argv (dry-run and real)
	Steps     []string  `json:"steps,omitempty"` // tui: the rendered frames (dry-run)
	Dir       string    `json:"dir,omitempty"`   // the workdir used
	Outcome   string    `json:"outcome"`         // success | error | unavailable | timeout | cancelled | dry-run
	Text      string    `json:"text,omitempty"`  // turn: final message; transcript: whole session text
	Artifacts []string  `json:"artifacts,omitempty"`
	Session   string    `json:"session,omitempty"` // tool session id when known
	ExitCode  int       `json:"exit_code"`
	Error     string    `json:"error,omitempty"`
	Started   time.Time `json:"started"`
	Duration  string    `json:"duration,omitempty"`
}

// ErrUnavailable marks a command the tool refused in the requested mode.
var ErrUnavailable = errors.New("tool command unavailable in this mode")

// Run executes cmd on tool with the caller's args text. It validates the
// tool's commands block first and refuses an invalid declaration.
func Run(ctx context.Context, tool fleet.Tool, cmd fleet.ToolCommand, args string, opts Options) (Result, error) {
	// Validate the command being run (in the tool's context, so a tui
	// command still sees steer_exec); other invalid entries do not block it.
	one := tool
	one.Commands = []fleet.ToolCommand{cmd}
	if errs, _ := one.ValidateCommands(); len(errs) > 0 {
		err := errors.Join(errs...)
		return Result{Tool: tool.Name, Command: cmd.Name, Mode: cmd.Mode, Outcome: OutcomeError, Error: err.Error()}, err
	}
	switch cmd.Mode {
	case fleet.ToolCommandPrint:
		return runPrint(ctx, tool, cmd, args, opts)
	case fleet.ToolCommandTUI:
		return runTUI(ctx, tool, cmd, args, opts)
	default:
		err := fmt.Errorf("toolcmd: %s:%s: unknown mode %q", tool.Name, cmd.Name, cmd.Mode)
		return Result{Tool: tool.Name, Command: cmd.Name, Mode: cmd.Mode, Outcome: OutcomeError, Error: err.Error()}, err
	}
}

// RenderSlash substitutes {args} in the slash line, or appends args after a
// space when the line has no {args} token.
func RenderSlash(slash, args string) string {
	args = strings.TrimSpace(args)
	if strings.Contains(slash, "{args}") {
		return strings.TrimSpace(strings.ReplaceAll(slash, "{args}", args))
	}
	if args == "" {
		return strings.TrimSpace(slash)
	}
	return strings.TrimSpace(slash) + " " + args
}

// CommandTimeout resolves the effective timeout: opts, then YAML, then default.
func CommandTimeout(cmd fleet.ToolCommand, opts Options) time.Duration {
	if opts.Timeout > 0 {
		return opts.Timeout
	}
	if cmd.Timeout != "" {
		if d, err := time.ParseDuration(cmd.Timeout); err == nil && d > 0 {
			return d
		}
	}
	return DefaultTimeout
}
