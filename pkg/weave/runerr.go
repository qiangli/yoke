package weave

import (
	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// installRunErrorReporting is the THIRD and last half of the
// self-reporting contract: flagerr.go covers flag-parse failures,
// argerr.go covers positional-argument failures, and this covers the
// errors a RunE returns ITSELF.
//
// Why this exists (todo 75d1842bc4c9). The first two reporters made
// cobra's own structural errors loud, and every subverb that reaches
// runWeaveStoryMutate/Read emits its own envelope. What neither covers
// is the GUARD — the check a RunE runs before it opens the store:
//
//	sprint goal rm 129 ci-sweep   -> exit 1, ZERO output (--reason required)
//	sprint handoff 136            -> exit 1, ZERO output (-m required)
//	sprint handoff notanumber -m x -> exit 1, ZERO output (bad sprint id)
//
// Those are ordinary errors returned from RunE, and SilenceErrors —
// set on every weave/sprint command so subverb envelopes are not
// double-printed — swallows them. A silent exit 1 is worse than an
// error, because an error stops you: the conductor of sprint #135 ran
// two `goal rm` calls, saw nothing, believed both goals were retired,
// and printed a "corrected" card that still carried them.
//
// The fix is deliberately STRUCTURAL rather than a message added to the
// two guards that were reported. There are a dozen such guards in the
// sprint tree today (`grep "is required" pkg/weave`), they are added
// routinely, and a guard written next year must be loud without its
// author knowing this file exists. So the report is installed once, by
// walking the tree, and any guard that returns a bare error gets it.
//
// Errors that are ALREADY structured pass through untouched: those
// subverbs emitted their own envelope, and reporting again would
// double-print the thing flagerr.go was careful to avoid.
func installRunErrorReporting(root *cobra.Command) {
	if root.RunE != nil {
		root.RunE = reportingRunE(root.RunE)
	}
	for _, sub := range root.Commands() {
		installRunErrorReporting(sub)
	}
}

// reportingRunE wraps one RunE so a bare error becomes a named,
// non-empty message on the command's own stderr plus a structured exit.
func reportingRunE(orig func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := orig(cmd, args)
		if err == nil || IsStructuredExit(err) {
			return err
		}
		// A guard rejects the INVOCATION, so this is a usage failure
		// (exit 2) — the same code ExitCode() already assigns to an
		// unstructured error, which keeps the process's exit status
		// identical to today's. Only the silence changes.
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), flagErrOutputMode(cmd),
			cmd.CommandPath(), weavecli.ExitInvalidArg, err))
	}
}
