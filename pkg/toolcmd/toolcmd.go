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
	// Argv is the caller's args as an argv (e.g. the words after `--` on the
	// CLI). A print command's `exec` template expands {args} to these tokens
	// verbatim; when nil, the args string is split with shell quoting rules.
	// Slash lines always use the args string.
	Argv []string
}

// Result is the structured envelope of one command run.
type Result struct {
	Tool      string   `json:"tool"`
	Command   string   `json:"command"`
	Mode      string   `json:"mode"`
	Slash     string   `json:"slash,omitempty"`   // the rendered slash line
	Argv      []string `json:"argv,omitempty"`    // print: the rendered argv (dry-run and real)
	Steps     []string `json:"steps,omitempty"`   // tui: the rendered frames (dry-run)
	Dir       string   `json:"dir,omitempty"`     // the workdir used
	Outcome   string   `json:"outcome"`           // success | error | unavailable | timeout | cancelled | dry-run
	Verdict   string   `json:"verdict,omitempty"` // the tool's own terminal verdict (events_outcome): succeeded | unverified
	Text      string   `json:"text,omitempty"`    // turn: final message; transcript: whole session text
	Artifacts []string `json:"artifacts,omitempty"`
	Session   string   `json:"session,omitempty"` // tool session id when known
	ExitCode  int      `json:"exit_code"`
	Error     string   `json:"error,omitempty"`
	// Refusal names a guard that refused the launch (RefusalLaunchGuard,
	// RefusalAgentLive) and Hint the legitimate ways forward. Set by Run.
	Refusal  string    `json:"refusal,omitempty"`
	Hint     string    `json:"hint,omitempty"`
	Started  time.Time `json:"started"`
	Duration string    `json:"duration,omitempty"`
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
	var res Result
	var err error
	switch cmd.Mode {
	case fleet.ToolCommandPrint:
		res, err = runPrint(ctx, tool, cmd, args, opts)
	case fleet.ToolCommandTUI:
		res, err = runTUI(ctx, tool, cmd, args, opts)
	default:
		err := fmt.Errorf("toolcmd: %s:%s: unknown mode %q", tool.Name, cmd.Name, cmd.Mode)
		return Result{Tool: tool.Name, Command: cmd.Name, Mode: cmd.Mode, Outcome: OutcomeError, Error: err.Error()}, err
	}
	if err != nil {
		if refusal, hint := classifyRefusal(err); refusal != "" {
			res.Refusal, res.Hint = refusal, hint
			err = &RefusalError{Kind: refusal, Hint: hint, Err: err}
			res.Error = err.Error()
		}
	}
	return res, err
}

// Refusal kinds.
const (
	// RefusalLaunchGuard: the tool's launch carries an approval-gate
	// kill-switch (e.g. --dangerously-skip-permissions) and nothing contains
	// it. toolcmd never sets or implies the bypass.
	RefusalLaunchGuard = "launch-guard"
	// RefusalAgentLive: the agent identity already has a live session; an
	// agent is a singleton, so a second one is refused, not queued.
	RefusalAgentLive = "agent-live"
)

// RefusalError is a launch a guard refused, with the ways forward.
type RefusalError struct {
	Kind string
	Hint string
	Err  error
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("toolcmd: refused (%s): %v\n%s", e.Kind, e.Err, e.Hint)
}
func (e *RefusalError) Unwrap() error { return e.Err }

const (
	hintLaunchGuard = "ways out: run it contained (bashy contain -- bashy tool cmd run ..., or a Bash# @contain fence), " +
		"or the operator explicitly accepts the risk by setting BASHY_ALLOW_UNSAFE_AGENT_LAUNCH=1 in the environment. " +
		"bashy tool cmd has no flag that bypasses the guard."
	hintAgentLive = "ways out: pass --agent NAME with a clone identity (bashy agent clone AGENT), " +
		"or attach to the live session (bashy chat --agent AGENT --attach), or wait for it to end."
)

// classifyRefusal recognises the two launch refusals by their stable
// wording in agentlaunch.GuardUnsafeArgs and chat.errAgentLive.
func classifyRefusal(err error) (kind, hint string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "agent launch: refusing to launch"):
		return RefusalLaunchGuard, hintLaunchGuard
	case strings.Contains(msg, "is already live") || strings.Contains(msg, "agent live"):
		return RefusalAgentLive, hintAgentLive
	}
	return "", ""
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
