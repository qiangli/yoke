package supervise

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// stdoutProgress narrates the run to the operator.
type stdoutProgress struct{ w io.Writer }

func (s stdoutProgress) progress(line string)  { fmt.Fprintln(s.w, line) }
func (s stdoutProgress) liveStream() io.Writer { return s.w }

// NewSuperviseCmd returns the `bashy supervise` command.
func NewSuperviseCmd() *cobra.Command {
	var (
		goal        string
		supervisor  string
		fleet       []string
		tasks       []string
		brief       []string
		maxAttempts int
		sandbox     string
		turnTimeout string
		keepGoing   bool
		out         string
		jsonOut     bool
		dry         bool
	)
	cmd := &cobra.Command{
		// A non-converged run returns an error (so scripts see a non-zero exit),
		// but that is a result, not a misuse — don't dump usage/help on it.
		SilenceUsage:  true,
		SilenceErrors: true,
		Use:           "supervise --goal TEXT --worker AGENT --task 'goal [:: gate]' ...",
		Short:         "drive a fleet of agents against a goal with optional deterministic gates",
		Long: "Drive worker agents against a goal decomposed into tasks, IN the current\n" +
			"working tree (the in-place counterpart to `bashy weave`'s isolated workspaces —\n" +
			"use this when work spans a sibling repo or gitignored\n" +
			"assets weave can't see). When supplied, a task's `:: <gate>` is a shell command\n" +
			"the orchestrator runs after the worker's turn and its exit code is authoritative.\n" +
			"Without a gate, the worker exit decides the task and is marked UNVERIFIED. Retries\n" +
			"land on a different fleet member. --supervisor optionally adds a final summary;\n" +
			"it is never required and never determines convergence.",
		RunE: func(cmd *cobra.Command, args []string) error {
			contracts, err := parseTasks(tasks)
			if err != nil {
				return err
			}
			cwd, _ := os.Getwd()
			p := &Plan{
				Goal: goal, Supervisor: supervisor, Fleet: fleet, Contracts: contracts,
				Brief: brief, MaxAttempts: maxAttempts, Sandbox: sandbox, TurnTimeout: turnTimeout,
				KeepGoing: keepGoing, Cwd: cwd, Out: out, Created: nowFn(),
			}
			p.ID = newID(p.Goal, p.Created)
			if err := p.Validate(); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			printPreview(w, p)
			if dry {
				fmt.Fprintln(w, "(dry-run: no agents launched)")
				return nil
			}
			res, err := Run(cmd.Context(), p, nil, stdoutProgress{w})
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return err
				}
			} else {
				writeSummary(w, p, res)
			}
			if !res.Converged {
				return fmt.Errorf("supervise: not converged (%d/%d tasks passed) — see %s",
					passCount(res), len(p.Contracts), redactHome(res.Report))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&goal, "goal", "", "the overall goal (required)")
	f.StringVar(&supervisor, "supervisor", "", "agent that adds an optional final summary (never affects convergence)")
	f.StringArrayVar(&fleet, "worker", nil, "worker agent — decides content (repeatable, required)")
	f.StringArrayVar(&tasks, "task", nil, "a task as 'goal [:: gate-shell-command]' (repeatable; gate optional)")
	f.StringArrayVar(&brief, "brief", nil, "file handed to every worker as context (repeatable)")
	f.IntVar(&maxAttempts, "max-attempts", 3, "gate-fail retries per task, each on the next fleet member")
	f.StringVar(&sandbox, "sandbox", "", "agent sandbox (e.g. danger-full-access for full write/push access)")
	f.StringVar(&turnTimeout, "turn-timeout", "30m", "per-agent-turn timeout")
	f.BoolVar(&keepGoing, "keep-going", false, "continue to the next task after one fails to converge")
	f.StringVar(&out, "out", "docs", "report target: docs | <path>")
	f.BoolVar(&jsonOut, "json", false, "emit the machine-readable result envelope")
	f.BoolVar(&dry, "dry-run", false, "print the resolved plan and exit")
	return cmd
}

// parseTasks turns `--task "goal :: gate"` strings into contracts. The gate is
// everything after the first ` :: `. A pinned worker is `@agent: goal :: gate`.
func parseTasks(raw []string) ([]*Contract, error) {
	var out []*Contract
	for i, t := range raw {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		c := &Contract{ID: fmt.Sprintf("t%d", i+1)}
		// optional @worker: prefix
		if strings.HasPrefix(t, "@") {
			if head, rest, ok := strings.Cut(t, ":"); ok && !strings.Contains(head, " ") {
				c.Worker = strings.TrimSpace(strings.TrimPrefix(head, "@"))
				t = strings.TrimSpace(rest)
			}
		}
		goal, gate, ok := strings.Cut(t, " :: ")
		c.Goal = strings.TrimSpace(goal)
		if ok {
			c.Gate = strings.TrimSpace(gate)
		}
		// A named id may lead: "id| goal :: gate"
		if id, rest, ok := strings.Cut(c.Goal, "| "); ok && !strings.Contains(id, " ") {
			c.ID = strings.TrimSpace(id)
			c.Goal = strings.TrimSpace(rest)
		}
		if c.Goal == "" {
			return nil, fmt.Errorf("supervise: --task %d has no goal", i+1)
		}
		out = append(out, c)
	}
	return out, nil
}

func printPreview(w io.Writer, p *Plan) {
	fmt.Fprintln(w, "supervise: resolved plan")
	fmt.Fprintf(w, "  id          %s\n", p.ID)
	fmt.Fprintf(w, "  goal        %s\n", p.Goal)
	if strings.TrimSpace(p.Supervisor) != "" {
		fmt.Fprintf(w, "  supervisor  %s (optional summary)\n", p.Supervisor)
	}
	fmt.Fprintf(w, "  fleet       %s\n", strings.Join(p.Fleet, ", "))
	fmt.Fprintf(w, "  attempts    %d per task, rotating the fleet on retry\n", p.maxAttempts())
	if p.Sandbox != "" {
		fmt.Fprintf(w, "  sandbox     %s\n", p.Sandbox)
	}
	if len(p.Brief) > 0 {
		fmt.Fprintf(w, "  brief       %s\n", strings.Join(p.Brief, ", "))
	}
	fmt.Fprintln(w, "  tasks:")
	for _, c := range p.Contracts {
		pin := ""
		if c.Worker != "" {
			pin = " @" + c.Worker
		}
		gate := c.Gate
		if gate == "" {
			gate = "(no gate — UNVERIFIED)"
		}
		fmt.Fprintf(w, "    %s%s: %s\n        gate: %s\n", c.ID, pin, oneLine(c.Goal), oneLine(gate))
	}
	fmt.Fprintf(w, "  report →    %s\n", redactHome(p.reportPath()))
}

func writeSummary(w io.Writer, p *Plan, res *Result) {
	fmt.Fprintln(w)
	if res.Converged {
		fmt.Fprintf(w, "✅ CONVERGED — all %d tasks passed\n", len(p.Contracts))
	} else {
		fmt.Fprintf(w, "⚠ NOT CONVERGED — %d/%d tasks passed\n", passCount(res), len(p.Contracts))
	}
	for _, v := range res.Verdicts {
		mark := "✗"
		if v.Passed {
			mark = "✓"
		} else if v.Unverified {
			mark = "·"
		}
		fmt.Fprintf(w, "  %s %-14s %s (%d attempt(s))\n", mark, v.Contract, v.Worker, v.Attempts)
	}
	if s := strings.TrimSpace(res.Judgment); s != "" {
		fmt.Fprintf(w, "\nsupervisor: %s\n", s)
	}
	fmt.Fprintf(w, "\nreport: %s\n", redactHome(res.Report))
}

func passCount(res *Result) int {
	n := 0
	for _, v := range res.Verdicts {
		if v.Passed {
			n++
		}
	}
	return n
}
