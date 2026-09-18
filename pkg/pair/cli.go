package pair

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/gate"
)

// ExitProved is returned when the pair PROVED a defect: the gate was green, the pair acted,
// and now it is red.
//
// It is distinct from 1 (the pair or the plumbing failed) for the same reason judge's
// ExitBlocked is: a conductor must RETRY the second and must NOT retry the first. Retrying a
// proof just re-proves it.
const ExitProved = 4

// ExitBrokenBefore is returned when the gate was already red before the pair started.
// Nothing the pair did can be attributed. Fix the baseline, then pair.
const ExitBrokenBefore = 5

// ExitUnmeasured is returned when the after gate was red but no baseline gate was measured.
// The work is red, but its before state is unknown.
const ExitUnmeasured = 7

// NewPairCmd builds `bashy pair`.
//
// It REPLACES `bashy judge`, which could only ever talk. `judge` remains as a hidden alias
// mapping to `--role refute`, so existing scripts and the steward skill keep working.
func NewPairCmd() *cobra.Command {
	var (
		roleName    string
		proposer    string
		pairAgent   string
		gateOvr     string
		diffRef     string
		file        string
		timeout     time.Duration
		asJSON      bool
		listRoles   bool
		allowUnsafe bool
	)

	cmd := &cobra.Command{
		Use:   "pair [task]",
		Short: "run two agents in complementary roles, with optional command verification",
		Long: `Run work through two agents in different roles, optionally followed by a real gate.

The pair ACTS — it has the keyboard. It writes the failing test, or the fix, or its own
independent implementation. That is the difference between this and a reviewer:

    a critic's finding is a CLAIM.  Someone must now adjudicate it.
    a pair's failing test is a PROOF.  The gate reads it. Nobody adjudicates.

The pair may never approve. Without --verify, its result is an ungated advisory claim.
When --verify is supplied, that command decides the verified outcome.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if listRoles {
				return printRoles(cmd)
			}
			task := strings.TrimSpace(strings.Join(args, " "))
			return runPair(cmd, task, roleName, proposer, pairAgent, gateOvr, diffRef, file, timeout, asJSON, allowUnsafe)
		},
	}

	f := cmd.Flags()
	f.StringVar(&roleName, "role", "break", "what the pair does: "+strings.Join(RoleNames(), ", "))
	f.StringVar(&proposer, "proposer", "", "agent that WRITES the work (omit to attack work that already exists)")
	f.StringVar(&diffRef, "diff", "", "attack this diff (a git ref; default HEAD when the tree is dirty)")
	f.StringVar(&file, "file", "", "attack the contents of this file")
	f.StringVar(&pairAgent, "pair", "", "agent that pairs with it — MUST be a different model family")
	// --agent is the orchestration-facing spelling. Keep --pair as the
	// human-facing original; both feed the same identity and therefore the same
	// separation-of-duties checks in NewPlan.
	f.StringVar(&pairAgent, "agent", "", "agent that pairs with it (alias for --pair)")
	f.StringVar(&gateOvr, "verify", "", "run this authoritative gate command after pairing")
	f.DurationVar(&timeout, "timeout", 20*time.Minute, "per-agent timeout")
	f.BoolVar(&asJSON, "json", false, "emit "+SchemaVersion)
	f.BoolVar(&listRoles, "roles", false, "list the available roles and what each produces")
	f.BoolVar(&allowUnsafe, "yolo", false, "disable the acting agent's approval gate and accept the uncontained-host risk")
	return cmd
}

func printRoles(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%-16s %-6s %-8s %-8s %s\n", "ROLE", "ACTS", "SEES", "EVIDENCE", "WHAT IT DOES")
	for _, n := range RoleNames() {
		r := BuiltinRoles[n]
		acts, sees := "no", "yes"
		if r.Acts {
			acts = "YES"
		}
		if !r.SeesProposal {
			sees = "BLIND"
		}
		fmt.Fprintf(out, "%-16s %-6s %-8s %-8s %s\n", r.Name, acts, sees, r.Evidence, r.Summary)
	}
	fmt.Fprintln(out, "\nEvidence: diff > probe > verdict. A `diff` can be RUN, so it proves itself.")
	fmt.Fprintln(out, "A `verdict` is prose — a claim someone must decide whether to believe.")
	return nil
}

func runPair(cmd *cobra.Command, task, roleName, proposer, pairAgent, gateOvr, diffRef, file string, timeout time.Duration, asJSON, allowUnsafe bool) error {
	if task == "" {
		return fmt.Errorf("pair: a task is required")
	}
	role, err := ResolveRole(roleName)
	if err != nil {
		return err
	}

	cat := fleet.New()

	// Resolve the gate ONCE, BEFORE the pair gets write access.
	//
	// This is not caching. An acting pair can edit files, and `.bashy/gate` is a file — so a
	// pair that re-resolved the gate after acting could be graded by a gate IT WROTE. Capture
	// the definition up front and run that same definition both times; the pair may change
	// the code, never the ruler.
	var def *gate.Definition
	if strings.TrimSpace(gateOvr) != "" {
		root, _ := os.Getwd()
		def, err = gate.Resolve(root, gateOvr)
		if err != nil {
			return fmt.Errorf("pair: resolve --verify: %w", err)
		}
	}

	gateCmd := ""
	if def != nil {
		gateCmd = strings.Join(def.Commands, " && ")
	}

	plan, err := NewPlan(cat, role, Agents{Proposer: proposer, Pair: pairAgent}, task, gateCmd)
	if err != nil {
		return err
	}

	// No proposer means the work already exists. Find it.
	if proposer == "" {
		plan.Proposal, err = existingWork(diffRef, file)
		if err != nil {
			return err
		}
	}
	if !plan.Diverse {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", plan.Note)
	}

	ctx := cmd.Context()

	// The Runner. ReadOnly is the INVERSE of the role's agency — and that inversion is the
	// whole feature. judge hardcoded ReadOnly:true because a judge can APPROVE, and an agent
	// that can both write and approve will fix the code and then bless its own fix.
	//
	// A pair cannot approve. So it can safely be given the keyboard. Take away the authority
	// and the shackle is no longer load-bearing.
	run := func(ctx context.Context, agent, instruction string, acts bool) (string, error) {
		res, err := chat.Invoke(ctx, chat.Options{
			Agent:       agent,
			Role:        "pair",
			Instruction: instruction,
			Timeout:     timeout,
			ReadOnly:    !acts,
			AllowUnsafe: allowUnsafe && acts,
		}, nil)
		if err != nil {
			return "", err
		}
		// An agent that exited nonzero with nothing to say did not do the work. Do NOT let
		// its silence read as "found nothing" — that is a conclusion drawn from an absence.
		if res.ExitCode != 0 && strings.TrimSpace(res.Output) == "" {
			return "", fmt.Errorf("%s exited %d with no output", agent, res.ExitCode)
		}
		return res.Output, nil
	}

	runGate := func(ctx context.Context, _ string) (*GateRun, error) {
		r, err := gate.Run(ctx, def, "")
		if err != nil {
			return nil, err
		}
		return &GateRun{
			Command:   gateCmd,
			Passed:    r.Passed,
			Abstained: r.Verdict == gate.VerdictAbstained,
			ExitCode:  gateExit(r),
			Output:    failedChecks(r),
		}, nil
	}
	if def == nil {
		runGate = nil
	}

	res, err := plan.Run(ctx, run, runGate)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		render(cmd, res)
	}

	// Exit codes are DISTINCT because a conductor must react to them differently.
	//
	//   ExitProved       the pair proved a defect. Do NOT retry -- retrying just re-proves it.
	//                    Send it back to the proposer with the failing test attached.
	//   ExitBrokenBefore the baseline was already red. The pair's work is unattributable.
	//                    Fix the baseline, then pair.
	//   ExitUnmeasured   no baseline was measured and the after gate was red. The work is
	//                    red, but its before state is unknown.
	//   1                the plumbing failed. Retry is legitimate.
	//
	// Collapsing these into "nonzero" is how a conductor retries a proof nine times.
	switch res.Outcome {
	case OutcomeProved:
		os.Exit(ExitProved)
	case OutcomeBrokenBefore:
		os.Exit(ExitBrokenBefore)
	case OutcomeUnmeasured:
		os.Exit(ExitUnmeasured)
	}
	return nil
}

// existingWork gathers the work the pair will attack, when no proposer runs.
func existingWork(diffRef, file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("--- %s ---\n%s", file, b), nil
	}

	args := []string{"diff"}
	if diffRef != "" {
		args = append(args, diffRef)
	}
	out, err := exec.Command("git", args...).Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return string(out), nil
	}
	// A clean tree with no ref given: attack the last commit. That is the change a human
	// means when they say "break what I just did".
	if diffRef == "" {
		out, err = exec.Command("git", "show", "--format=%s%n", "HEAD").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return string(out), nil
		}
	}
	return "", ErrNoWork
}

func render(cmd *cobra.Command, r *Result) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\n%s\n\n", r.Headline())
	fmt.Fprintf(out, "  role      %s (%s evidence, acts=%v)\n", r.Role, r.Evidence, r.Acts)
	fmt.Fprintf(out, "  proposer  %s\n", r.Proposer)
	fmt.Fprintf(out, "  pair      %s\n", r.Pair)
	if r.GateBefore != nil {
		fmt.Fprintf(out, "  gate      before=%s  after=%s\n", gateWord(r.GateBefore), gateWord(r.GateAfter))
	} else if r.GateAfter != nil {
		fmt.Fprintf(out, "  gate      %s\n", gateWord(r.GateAfter))
	} else {
		fmt.Fprintln(out, "  gate      not run (ungated advisory result)")
	}
	if r.DiversityNote != "" {
		fmt.Fprintf(out, "\n  ! %s\n", r.DiversityNote)
	}
	if s := strings.TrimSpace(r.Contribution); s != "" {
		fmt.Fprintf(out, "\n--- what the pair did ---\n%s\n", s)
	}
}

// failedChecks reports WHICH checks failed. A gate that says only "RED" tells the next
// reader nothing they can act on.
func failedChecks(r *gate.Result) string {
	var b strings.Builder
	if r.Verdict == gate.VerdictAbstained {
		for _, c := range r.Checks {
			if c.Passed && c.OutputBytes == 0 {
				fmt.Fprintf(&b, "ABSTAIN (exit 0, no output): %s\n", c.Command)
			}
		}
		if r.Probe.Ran && !r.Probe.Proved {
			fmt.Fprintf(&b, "ABSTAIN (mutation probe unproved): %s\n", r.Probe.Error)
		}
	}
	for _, c := range r.Checks {
		if !c.Passed {
			fmt.Fprintf(&b, "FAIL (exit %d): %s\n%s\n", c.Exit, c.Command, strings.TrimSpace(c.Output))
		}
	}
	return strings.TrimSpace(b.String())
}

func gateWord(r *GateRun) string {
	if r.Abstained {
		return "ABSTAIN"
	}
	if r.Passed {
		return "green"
	}
	return "RED"
}

func gateExit(r *gate.Result) int {
	if r.Passed {
		return 0
	}
	if r.Verdict == gate.VerdictAbstained {
		return 2
	}
	return 1
}
