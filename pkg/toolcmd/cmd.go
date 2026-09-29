package toolcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
)

// NewCmd builds the `cmd` verb tree, mounted under the fleet `tool` noun as
// `bashy tool cmd`: list, run (sync, --dry-run, --async), and the async job
// verbs wait, result, cancel, jobs.
func NewCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "cmd",
		Short: "Run a tool's declared vendor commands (claude:deep-research, codex:review, ...)",
		Long: `Tool commands expose a tool's own vendor features — slash commands, skills,
subcommands — as named services, declared in the tool's fleet YAML under
commands: and keyed by a canonical cross-tool name (TOOL:NAME).

  bashy tool cmd list [TOOL]
  bashy tool cmd run TOOL:NAME [--dry-run|--json|--async] -- ARGS...
  bashy tool cmd wait|result|cancel JOB
  bashy tool cmd jobs

Async jobs are detached runner processes (no daemon); their state lives in
$BASHY_HOME/toolcmd/jobs/<id>/ (else ~/.bashy/toolcmd/jobs/<id>/).
See docs/tool-commands-design.md in the dhnt umbrella.`,
		SilenceUsage: true,
	}
	jobSelfArgv = func() ([]string, error) {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return append([]string{exe}, strings.Fields(root.CommandPath())...), nil
	}
	root.AddCommand(newListCmd(), newRunCmd(), newWaitCmd(), newResultCmd(), newCancelCmd(), newJobsCmd(), newJobRunnerCmd())
	return root
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// listRow is one declared command with its availability probe.
type listRow struct {
	Ref       string `json:"ref"`
	Tool      string `json:"tool"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	Slash     string `json:"slash"`
	Exec      string `json:"exec,omitempty"`
	Available bool   `json:"available"`
	Why       string `json:"why,omitempty"`
	Invalid   string `json:"invalid,omitempty"`
}

// lookPath is the availability probe (binary on PATH). A seam for tests.
var lookPath = exec.LookPath

func commandRows(tools []fleet.Tool) []listRow {
	var rows []listRow
	for _, t := range tools {
		for _, c := range t.Commands {
			r := listRow{Ref: t.Name + ":" + c.Name, Tool: t.Name, Name: c.Name, Mode: c.Mode, Slash: c.Slash, Exec: c.Exec, Available: true}
			one := t
			one.Commands = []fleet.ToolCommand{c}
			if errs, _ := one.ValidateCommands(); len(errs) > 0 {
				r.Invalid = errors.Join(errs...).Error()
				r.Available, r.Why = false, "invalid declaration"
			} else if _, err := lookPath(t.Binary()); err != nil {
				r.Available, r.Why = false, t.Binary()+" not on PATH"
			}
			rows = append(rows, r)
		}
	}
	return rows
}

func newListCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "list [TOOL]",
		Short: "List declared tool commands with an availability probe",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := fleet.New()
			var tools []fleet.Tool
			if len(args) == 1 {
				t, ok := cat.Tool(args[0])
				if !ok {
					return fmt.Errorf("unknown tool %q (see `bashy tool list`)", args[0])
				}
				tools = []fleet.Tool{t}
			} else {
				tools, _ = cat.Tools(true)
			}
			rows := commandRows(tools)
			out := cmd.OutOrStdout()
			if asJSON {
				if rows == nil {
					rows = []listRow{}
				}
				return writeJSON(out, rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "no tool commands declared (add a commands: block to a tool YAML; `bashy tool edit TOOL`)")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "COMMAND\tMODE\tAVAILABLE\tLINE")
			for _, r := range rows {
				avail := "yes"
				if !r.Available {
					avail = "no (" + r.Why + ")"
				}
				line := r.Slash
				if r.Exec != "" {
					line = r.Exec
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Ref, r.Mode, avail, line)
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

// shellQuote renders argv for display only.
func shellQuote(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`;&|<>(){}*?[]#~") {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

func newRunCmd() *cobra.Command {
	var (
		asJSON, dryRun, async bool
		dir, model, agent     string
		timeout               time.Duration
	)
	c := &cobra.Command{
		Use:   "run TOOL:NAME [-- ARGS...]",
		Short: "Run one tool command (sync, --dry-run, or --async)",
		Example: `  bashy tool cmd run codex:review --dry-run -- --uncommitted
  bashy tool cmd run claude:deep-research --async -- "why do tides lag the moon"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, rest := args[0], args[1:]
			opts := Options{Dir: dir, Timeout: timeout, Model: model, Agent: agent, DryRun: dryRun}
			if len(rest) > 0 {
				opts.Argv = rest
			}
			text := strings.Join(rest, " ")
			out := cmd.OutOrStdout()
			if async && !dryRun {
				j, err := StartJob(ref, text, opts)
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(out, j)
				}
				fmt.Fprintln(out, j.ID)
				return nil
			}
			tool, tc, err := lookupCommand(ref)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, runErr := runCommand(ctx, tool, tc, text, opts)
			if asJSON {
				if err := writeJSON(out, res); err != nil {
					return err
				}
				return runErr
			}
			if dryRun {
				if len(res.Argv) > 0 {
					fmt.Fprintln(out, shellQuote(res.Argv))
				}
				for _, s := range res.Steps {
					fmt.Fprintln(out, s)
				}
				if res.Dir != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "# mode %s, dir %s, line %q\n", res.Mode, res.Dir, res.Slash)
				}
				return runErr
			}
			if res.Text != "" {
				fmt.Fprintln(out, res.Text)
			}
			for _, a := range res.Artifacts {
				fmt.Fprintln(out, a)
			}
			return runErr
		},
	}
	f := c.Flags()
	f.BoolVar(&asJSON, "json", false, "emit the structured result envelope")
	f.BoolVar(&dryRun, "dry-run", false, "print the rendered argv (print) or steps (tui) without running")
	f.BoolVar(&async, "async", false, "start a detached job and print its id")
	f.StringVar(&dir, "dir", "", "working directory (default: a fresh temp dir)")
	f.StringVar(&model, "model", "", "model to bind (TOOL:MODEL)")
	f.StringVar(&agent, "agent", "", "agent nick to resolve the launch from (default: the tool)")
	f.DurationVar(&timeout, "timeout", 0, "override the command's timeout")
	return c
}

func printJob(w io.Writer, j Job, asJSON bool) error {
	if asJSON {
		return writeJSON(w, j)
	}
	line := fmt.Sprintf("%s  %s:%s  %s", j.ID, j.Tool, j.Command, j.Status)
	if j.Outcome != "" {
		line += "  outcome=" + j.Outcome
	}
	if j.Error != "" {
		line += "  error=" + j.Error
	}
	_, err := fmt.Fprintln(w, line)
	return err
}

func jobErr(j Job) error {
	switch j.Status {
	case JobDone:
		return nil
	case JobFailed:
		return fmt.Errorf("job %s failed: %s", j.ID, j.Error)
	default:
		return fmt.Errorf("job %s %s", j.ID, j.Status)
	}
}

func newWaitCmd() *cobra.Command {
	var asJSON bool
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "wait JOB",
		Short: "Wait for an async job to finish",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			j, err := WaitJob(ctx, args[0], 0)
			if err != nil {
				return err
			}
			if err := printJob(cmd.OutOrStdout(), j, asJSON); err != nil {
				return err
			}
			return jobErr(j)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the job record as JSON")
	c.Flags().DurationVar(&timeout, "timeout", 0, "give up waiting after this long (0 = no limit)")
	return c
}

func newResultCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "result JOB",
		Short: "Print a finished job's result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := ReadResult(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, r)
			}
			if r.Text != "" {
				fmt.Fprintln(out, r.Text)
			}
			for _, a := range r.Artifacts {
				fmt.Fprintln(out, a)
			}
			if r.Outcome != OutcomeSuccess {
				return fmt.Errorf("outcome %s: %s", r.Outcome, r.Error)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the result envelope")
	return c
}

func newCancelCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "cancel JOB",
		Short: "Cancel an async job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			j, err := CancelJob(ctx, args[0], 10*time.Second)
			if err != nil {
				return err
			}
			return printJob(cmd.OutOrStdout(), j, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the job record as JSON")
	return c
}

func newJobsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "jobs",
		Short: "List async jobs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			js, err := ListJobs()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if js == nil {
					js = []Job{}
				}
				return writeJSON(out, js)
			}
			for _, j := range js {
				if err := printJob(out, j, false); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

// newJobRunnerCmd is the hidden entry point of the detached runner process.
func newJobRunnerCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_job JOB",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return RunJob(ctx, args[0])
		},
	}
}
