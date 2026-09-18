// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package gate

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// ExitFailed is the exit code when the gate does not pass. Distinct from 1 (an
// internal error) so a caller can tell "the project failed" from "the gate itself
// broke" — a distinction weave and CI both need and neither could make before.
const ExitFailed = 3

// NewGateCmd builds `bashy gate`: does this project pass?
func NewGateCmd() *cobra.Command {
	var (
		command string
		asJSON  bool
		show    bool
	)
	cmd := &cobra.Command{
		Use:   "gate",
		Short: "does this project pass? (the one command that decides)",
		Long: `gate runs the project's gate — the command that decides pass/fail — and
returns the verdict.

It exists because bashy had no Test verb. Not because nobody tested, but because
the gate was spelled FOUR incompatible ways, each privately:

    weave      a shell command in .agents/weave/suite-gate
    sdlc       a healthcheck: key in sdlc.yaml
    supervise  a ::-delimited string inside --task 'goal :: gate'
    dag        a target that happens to fail

All four mean the same thing: run a command, and let its exit status be the
verdict. They never disagreed about semantics — only about where the command
lives. So there was no way to ask "does this project pass?" from a command line,
no shared result schema, and four places to change when the answer changed.

Define it once:

    echo 'make test' > .bashy/gate

One command per line; blank lines and #-comments ignored. It stops at the first
failure — a gate is a decision, not a test report, and the decision is made the
moment one check fails.

A project with no gate is an ERROR, not a pass. It has not passed; it has failed
to say what passing MEANS. That is how a green check mark comes to mean nothing.`,
		Example: `  bashy gate                    # does this project pass?
  bashy gate --json             # the verdict, for an agent
  bashy gate --show             # what WOULD run, without running it
  bashy gate --command 'go test ./...'   # a one-off gate`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root := repoRoot(cwd)

			def, err := Resolve(root, command)
			if err != nil {
				return err
			}

			if show {
				fmt.Fprintf(cmd.OutOrStdout(), "gate: %s (from %s)\n", def.Root, def.Source)
				for _, c := range def.Commands {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", c)
				}
				return nil
			}

			res, err := Run(cmd.Context(), def, os.Getenv("SHELL"))
			if err != nil {
				return err
			}

			if asJSON {
				fmt.Fprintln(cmd.OutOrStdout(), res.JSON())
			} else {
				for _, c := range res.Checks {
					mark := "PASS"
					if !c.Passed {
						mark = "FAIL"
					} else if c.OutputBytes == 0 {
						mark = "ABSTAIN"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  (%s)\n", mark, c.Command, c.Duration)
					if !c.Passed && c.Output != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", c.Output)
					}
				}
				if res.Probe.Ran {
					fmt.Fprintf(cmd.OutOrStdout(), "  PROBE  %s  (proved=%t)\n", res.Probe.Name, res.Probe.Proved)
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "  PROBE  not run (opt-in)")
				}
				if res.Passed {
					fmt.Fprintf(cmd.OutOrStdout(), "\ngate: PASS (%s, from %s)\n", res.Duration, res.Source)
				} else if res.Verdict == VerdictAbstained {
					fmt.Fprintf(cmd.OutOrStdout(), "\ngate: ABSTAIN (%s, from %s)\n", res.Duration, res.Source)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "\ngate: FAIL (%s, from %s)\n", res.Duration, res.Source)
				}
			}

			if !res.Passed {
				// A distinct code, so a caller can tell "the project failed" from
				// "the gate itself broke". SilenceUsage/SilenceErrors: a failing gate
				// is a VERDICT, not a usage error — printing a usage block over a
				// build failure is noise at exactly the wrong moment.
				cmd.SilenceUsage, cmd.SilenceErrors = true, true
				os.Exit(ExitFailed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&command, "command", "", "run this instead of the project's gate")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the verdict as "+SchemaVersion)
	cmd.Flags().BoolVar(&show, "show", false, "print what would run, without running it")
	return cmd
}

func repoRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return dir
	}
	return strings.TrimSpace(string(out))
}
