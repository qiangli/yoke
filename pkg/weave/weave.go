package weave

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// newWeaveCmd builds the `bashy weave ...` top-level group — the v2
// human-facing front door per docs/loom-v2-plan.md. Subverbs
// dispatch through the agent-friendly envelope conventions in
// internal/cli/weavecli; concrete implementations land in
// per-subverb files (weave_add.go, weave_start.go, etc.) so this
// file stays a thin registry.
func newWeaveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "weave",
		Short: "Run agentic tools in isolated, convergent workspaces (v2)",
		// A command group with neither Run nor RunE is not Runnable, and
		// cobra bails out with flag.ErrHelp (command.go:955) BEFORE it ever
		// reaches ValidateArgs — so an Args validator here would be dead
		// code and bare `weave` would exit non-zero with no diagnostic,
		// indistinguishable from success to a caller that only checks
		// output. Giving the root a RunE makes it Runnable, so empty argv
		// lands here and emits the same structured envelope every subverb
		// does.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), weavecli.ResolveOutputMode(false, false, false),
				"weave", weavecli.ExitInvalidArg, fmt.Errorf("missing command; run 'weave --help'")))
		},
		// Every weave subverb emits its own structured envelope (or
		// human line) and propagates an *exitCodeError carrying a
		// stable weavecli exit code. cobra's default "Error: ..." +
		// usage dump would double-print on top of the envelope, so we
		// silence both at the parent level — subverbs inherit. This
		// also silences cobra's own structural errors (unknown
		// subcommand, bad flag), which never reach a subverb — so weave
		// reports those itself rather than leaving them to the host:
		// flags via SetFlagErrorFunc below (flagerr.go), positional
		// args via installArgsErrorReporting (argerr.go). Both emit an
		// envelope on this command's stderr and return a structured
		// exit, so a host that prints only when IsStructuredExit is
		// false (export.go) stays silent and never double-prints.
		SilenceErrors: true,
		SilenceUsage:  true,
		Long: `weave is the per-repo EXECUTION engine: a local, filesystem-based
orchestrator that runs agentic CLIs (codex, claude, agy, opencode, ...)
in parallel over ONE repo. Seed a queue of issues (runs), fan tools out
across them in isolated git-clone workspaces so they never clobber each
other, then pull the converged work back into the repo. Entirely local —
no server, no forge.

For the CROSS-REPO plan/handoff layer above weave — the sprint kanban,
conductor lease, continuity record, and checkpoints — see ` + "`bashy sprint`" + `.

Common-case usage:

  bashy weave add "fix null deref in cache" --priority p0
  bashy weave start -- codex --dangerously-skip-permissions "<body>"
  bashy weave list                           # runs in flight
  bashy weave pull                           # absorb merged work`,
	}

	// weave is an agent/orchestrator surface, not an interactive human
	// shell — the cobra-generated `completion` subverb (and its hidden
	// `__complete` helper) only add noise to `weave --help` and don't
	// fit the structured-envelope contract. Drop them.
	cmd.CompletionOptions.DisableDefaultCmd = true

	// A flag-parse error never reaches a subverb's RunE, so nothing in
	// the envelope path prints it — and SilenceErrors above hides
	// cobra's own message. That combination made a misspelled flag
	// (`weave baton write --note ...`) exit non-zero with ZERO output,
	// which is indistinguishable from success. cobra's FlagErrorFunc()
	// climbs to the parent, so installing it here covers EVERY subverb
	// in the tree at once. See flagerr.go.
	cmd.SetFlagErrorFunc(weaveFlagErrorFunc)

	cmd.AddCommand(newWeaveAddCmd())
	cmd.AddCommand(newWeaveSplitCmd())
	cmd.AddCommand(newWeaveLinkCmd())
	cmd.AddCommand(newWeaveStartCmd())
	cmd.AddCommand(newWeaveNextCmd())
	cmd.AddCommand(newWeavePrioCmd())
	cmd.AddCommand(newWeavePointCmd())
	cmd.AddCommand(newWeaveListCmd())
	cmd.AddCommand(newWeavePauseCmd())
	cmd.AddCommand(newWeaveResumeCmd())
	cmd.AddCommand(newWeaveAutopilotCmd())
	cmd.AddCommand(newWeaveFleetCmd())
	cmd.AddCommand(newWeaveStatusCmd())
	cmd.AddCommand(newWeaveLogCmd())
	cmd.AddCommand(newWeaveRememberCmd())
	cmd.AddCommand(newWeaveRecallCmd())
	cmd.AddCommand(newWeaveMemoryCmd())
	cmd.AddCommand(newWeaveAttachCmd())
	cmd.AddCommand(newWeaveCommentCmd())
	cmd.AddCommand(newWeaveCommentsCmd())
	cmd.AddCommand(newWeaveSayCmd())
	cmd.AddCommand(newWeavePullCmd())
	cmd.AddCommand(newWeaveReviewCmd())
	cmd.AddCommand(newWeaveReverifyCmd())
	cmd.AddCommand(newWeaveSalvageCmd())
	cmd.AddCommand(newWeavePruneCmd())
	cmd.AddCommand(newWeaveAbandonCmd())
	cmd.AddCommand(newWeaveKillCmd())
	cmd.AddCommand(newWeaveFinalizeCmd())
	cmd.AddCommand(newWeaveShellCmd())
	cmd.AddCommand(newWeaveOpenCmd())
	cmd.AddCommand(newWeaveResetCmd())
	cmd.AddCommand(newWeaveWaitCmd())
	cmd.AddCommand(newWeaveCheckCmd())
	cmd.AddCommand(newWeaveDoctorCmd())
	cmd.AddCommand(newWeaveGuideCmd())
	cmd.AddCommand(newWeaveHeartbeatCmd())
	// baton = the per-repo campaign single-driver lock (execution
	// coordination for THIS repo's queue) — stays in weave. The
	// cross-repo conductor-coordination verbs (cloudbox shared sessions
	// + conduct) moved to `bashy sprint` (the plan/handoff layer).
	cmd.AddCommand(newWeaveBatonCmd())

	// LAST, after the whole tree exists: give positional arguments the
	// same self-reporting treatment SetFlagErrorFunc gives flags.
	// Unlike FlagErrorFunc there is no inheritance to lean on — cobra has
	// no ArgsErrorFunc — so this walks every command and wraps its Args
	// validator in place. Covers the unknown subcommand (`weave bogus`,
	// and `weave baton bogus`, which cobra does not even check) and the
	// missing operand (`weave status`), both of which used to exit
	// non-zero with ZERO output. See argerr.go.
	installArgsErrorReporting(cmd)

	// ...and the errors a RunE returns itself: a guard that runs before
	// the store is opened is not a cobra structural error, so neither
	// reporter above sees it. See runerr.go.
	installRunErrorReporting(cmd)

	return cmd
}

// weaveOutputFlags adds the standard --json/--plain/--quiet flags
// shared across every subverb and returns getters so RunE bodies can
// resolve the OutputMode without re-declaring the flags.
type weaveOutputFlags struct {
	jsonF, plainF, quietF bool
	cmd                   *cobra.Command // for Flags().Changed("json") in mode()
}

func (f *weaveOutputFlags) attach(cmd *cobra.Command) {
	f.cmd = cmd
	cmd.Flags().BoolVar(&f.jsonF, "json", false, "Emit machine-readable envelope (versioned schema)")
	cmd.Flags().BoolVar(&f.plainF, "plain", false, "Plain-text output, no ANSI or spinners")
	cmd.Flags().BoolVar(&f.quietF, "quiet", false, "Final result line only")
	// Every subverb emits its own envelope and propagates an
	// *exitCodeError. cobra's error handling only consults the leaf
	// command and the absolute root — NOT the `weave` parent — so the
	// parent's SilenceErrors/SilenceUsage don't reach here. Set them on
	// the leaf itself or cobra double-prints "Error: exit N" + a full
	// usage dump on top of the envelope (the confirmation-refusal noise).
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
}

func (f *weaveOutputFlags) mode() weavecli.OutputMode {
	jsonSet := f.cmd != nil && f.cmd.Flags().Changed("json")
	return weavecli.ResolveOutputModeEx(jsonSet, f.jsonF, f.plainF, f.quietF)
}

// exitCodeError lets RunE propagate a specific exit code while cobra
// still sees an error (so its return-non-zero plumbing fires).
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit %d", e.code) }
func (e *exitCodeError) ExitCode() int { return e.code }
