// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package judge

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ExitBlocked is returned by `judge --gate` when the verdict is not "approve".
//
// Distinct from 1, so a caller can tell "the work was judged inadequate" from "the judge
// itself failed to run" — a distinction that matters enormously to an automated
// conductor, which must retry the second and must NOT retry the first.
const ExitBlocked = 4

// NewJudgeCmd builds `bashy judge`.
func NewJudgeCmd() *cobra.Command {
	var (
		stage    string
		diff     string
		file     string
		run      int64
		panelN   int
		agents   []string
		timeout  time.Duration
		gateMode bool
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "judge [--diff <ref> | --file <path> | --run <n>]",
		Short: "is this work any good? (the semantic twin of `gate`)",
		Long: `judge reads a piece of work and renders an opinion on it.

  gate   does it PASS?   mechanical, reproducible, safe to block a merge on
  judge  is it GOOD?     semantic, an opinion, advisory unless you ask otherwise

Together they encode in two commands what the conductor playbook keeps writing in
prose: SANDBOX-GREEN IS NOT MERGEABLE. A change can pass every test and still be
the wrong change.

Until now bashy could VERIFY but not JUDGE. 'weave review' sounds like this and
isn't -- it re-runs the verify command in a clean-room clone and never launches an
agent. Nothing in the tool ever read a diff, a plan or a failure and formed a view.
The role existed anyway, as ad-hoc prompting: this project's own JUDGE-REPORT-R6/R7
and QA-REPORT-R10 are its artifacts. This is the verb behind them.

THE PANEL IS INDEPENDENT, NOT DELIBERATIVE. Each reviewer sees the work cold and
never sees another's opinion. That is deliberate, and it is why this is not 'meet':
deliberation produces ANCHORING -- the first opinion voiced drags the rest -- and a
panel that converges is not the same as a panel that agrees. Reviewers are also
distinct TOOLS, never two models on one harness: those share the harness's blind
spots, so their agreement is not evidence.

ADVISORY BY DEFAULT. judge exits 0 and reports; --gate makes the verdict binding
(exit 4 unless approved). That default is not timidity: a gate is reproducible, an
LLM opinion is not, and one hallucinated "reject" wired into a merge would wedge a
fleet. You choose where an opinion is allowed to stop the line.

A FAILED REVIEWER IS NEVER AN APPROVAL. If a reviewer crashes, times out, or
returns nonsense, it has no vote and it is reported as an error -- it is never
counted as consent.`,
		Example: `  bashy judge                              # the working tree, as a code review
  bashy judge --diff main...HEAD --panel 3 # three independent reviewers
  bashy judge --file docs/plan.md --stage plan
  bashy judge --run 7 --gate               # judge a weave run; exit 4 unless approved`,
		RunE: func(cmd *cobra.Command, args []string) error {
			subject, content, st, err := gather(diff, file, run, stage)
			if err != nil {
				return err
			}
			if strings.TrimSpace(content) == "" {
				return fmt.Errorf("nothing to judge: %s is empty", subject)
			}

			picked, note, err := SelectPanel(panelN, agents)
			if err != nil {
				return err
			}
			if note != "" && !asJSON {
				fmt.Fprintf(cmd.ErrOrStderr(), "judge: %s\n", note)
			}
			if !asJSON {
				fmt.Fprintf(cmd.ErrOrStderr(), "judge: %s (%s) — %d reviewer(s): %s\n",
					subject, st, len(picked), strings.Join(picked, ", "))
			}

			rep := Panel(cmd.Context(), picked, st, subject, content, timeout)

			if run != 0 {
				recordOnRun(run, rep)
			}
			if asJSON {
				b, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				render(cmd, rep)
			}
			if rep.Verdict == Errored {
				// Every reviewer failed. That is a judge failure, not a bad verdict,
				// and a conductor must retry it rather than treat it as rejection.
				return fmt.Errorf("no reviewer returned a usable verdict (%d error(s))", rep.Errors)
			}
			if gateMode && rep.Blocking() {
				os.Exit(ExitBlocked)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stage, "stage", "", "plan|code|test|deploy — selects the rubric (default: code, or the run's stage)")
	cmd.Flags().StringVar(&diff, "diff", "", "judge a git diff (e.g. main...HEAD); default: the working tree")
	cmd.Flags().StringVar(&file, "file", "", "judge a file (a plan, a design, a failure log)")
	cmd.Flags().Int64Var(&run, "run", 0, "judge a weave run's work, and record the verdict on it")
	cmd.Flags().IntVar(&panelN, "panel", 1, "how many INDEPENDENT reviewers (distinct tools)")
	cmd.Flags().StringArrayVar(&agents, "agent", nil, "pin a reviewer (repeatable; overrides --panel selection)")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "per-reviewer timeout")
	cmd.Flags().BoolVar(&gateMode, "gate", false, "make the verdict BINDING: exit 4 unless approved")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the "+SchemaVersion+" report")
	return cmd
}

// gather turns the flags into the text a reviewer will actually read.
func gather(diff, file string, run int64, stage string) (subject, content, st string, err error) {
	switch {
	case file != "":
		b, e := os.ReadFile(file)
		if e != nil {
			return "", "", "", e
		}
		return file, string(b), NormalizeStage(stage), nil

	case run != 0:
		s, c, rs, e := gatherRun(run)
		if e != nil {
			return "", "", "", e
		}
		if stage == "" {
			stage = rs // a run knows which stage it serves; use it unless told otherwise
		}
		return s, c, NormalizeStage(stage), nil

	case diff != "":
		out, e := git("diff", diff)
		if e != nil {
			return "", "", "", e
		}
		return "diff " + diff, out, NormalizeStage(stage), nil

	default:
		// The working tree, staged and unstaged together — what a colleague would see
		// if they looked over your shoulder right now.
		out, e := git("diff", "HEAD")
		if e != nil {
			return "", "", "", e
		}
		return "the working tree", out, NormalizeStage(stage), nil
	}
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func render(cmd *cobra.Command, r Report) {
	w := cmd.OutOrStdout()
	mark := map[Verdict]string{Approve: "APPROVE", Revise: "REVISE", Reject: "REJECT", Errored: "ERROR"}
	fmt.Fprintf(w, "\n%s — %s\n", mark[r.Verdict], r.Subject)
	if len(r.Panel) > 1 {
		agree := "split"
		if r.Unanimous {
			agree = "unanimous"
		}
		fmt.Fprintf(w, "  %d reviewers, %s", len(r.Panel), agree)
		if r.Errors > 0 {
			fmt.Fprintf(w, ", %d errored (not counted as approval)", r.Errors)
		}
		fmt.Fprintln(w)
		for _, o := range r.Panel {
			if o.Verdict == Errored {
				fmt.Fprintf(w, "    %-14s error: %s\n", o.Agent, o.Error)
				continue
			}
			fmt.Fprintf(w, "    %-14s %s (%s)\n", o.Agent, o.Verdict, o.Took)
		}
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(w)
		for _, f := range r.Findings {
			loc := f.File
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			if loc != "" {
				loc = " " + loc
			}
			fmt.Fprintf(w, "  [%s]%s %s\n", f.Severity, loc, f.Summary)
		}
	}
	for _, o := range r.Panel {
		if o.Notes != "" {
			fmt.Fprintf(w, "\n  %s: %s\n", o.Agent, o.Notes)
		}
	}
	fmt.Fprintln(w)
}
