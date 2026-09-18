package weave

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// installArgsErrorReporting is the POSITIONAL-ARGUMENT half of the
// self-reporting contract weaveFlagErrorFunc (flagerr.go) gives flags.
//
// Why this exists: every weave command sets SilenceErrors/SilenceUsage
// because subverbs print their own envelope and cobra must not
// double-print. That silence also swallows the structural errors cobra
// raises from ValidateArgs — which never reach a RunE, so nothing prints
// them. Three concrete silent failures on the pre-fix tree:
//
//	weave bogusxyz42   -> exit 1, ZERO output (unknown command)
//	weave status       -> exit 1, ZERO output ("requires at least 1 arg(s)")
//	weave baton bogus  -> exit 0, ran `baton` and IGNORED the typo'd subverb
//
// The last one is the worst: cobra's legacyArgs only does the
// unknown-subcommand check on the ROOT command (args.go:35 guards on
// !cmd.HasParent()), so a nested group silently discarded the bad name
// and reported success. A conductor reading "exit 0" believed the
// handoff note was written.
//
// There are TWO ways a group swallows a typo'd subverb, and wrapping
// Args only closes the first. The second is that cobra never calls
// ValidateArgs at all on a group with no Run/RunE — see
// makeGroupRunnable below, which is what makes the wrapper reachable.
//
// FlagErrorFunc has no positional-argument twin in cobra, so instead of
// delegating the message to the host we wrap every command's Args
// validator once, at tree-construction time, and report failures the
// same way flag errors are reported: a structured envelope on the
// command's own stderr plus an *exitCodeError, so IsStructuredExit is
// true and the host stays silent. Exactly one message no matter who
// drives the tree.
func installArgsErrorReporting(root *cobra.Command) {
	root.Args = reportingArgs(root)
	makeGroupRunnable(root)
	for _, sub := range root.Commands() {
		installArgsErrorReporting(sub)
	}
}

// makeGroupRunnable is what makes the wrapper above REACHABLE on a
// group that has no Run/RunE of its own.
//
// cobra returns flag.ErrHelp for a non-runnable command BEFORE it ever
// calls ValidateArgs (command.go: `if !c.Runnable() { return
// flag.ErrHelp }` precedes the ValidateArgs call), and ExecuteC turns
// ErrHelp into "print help, return nil". So on a group with no RunE the
// Args wrapper is installed and never runs, and a typo'd subverb prints
// the group's help and EXITS 0 — reporting success for a command that
// did not exist. That is the worst of the three failures this file was
// written for, and it survived here because every weave group happened
// to have a RunE; `sprint session` does not, and it exited 0 on
// `sprint session bogusverb`.
//
// Giving the group a RunE that simply shows help makes it Runnable, so
// ValidateArgs runs and the wrapper reports the bad name. The no-args
// path is unchanged by construction: cobra printed help and exited 0
// before, and c.Help() prints help and exits 0 now.
func makeGroupRunnable(cmd *cobra.Command) {
	if !cmd.HasSubCommands() || cmd.Runnable() {
		return
	}
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		// Reached only with NO positional args: any leftover positional
		// on a group fails in ValidateArgs above, before this runs.
		return c.Help()
	}
}

// reportingArgs wraps cmd's existing Args validator (captured now, so
// the wrapper is not self-recursive) with the unknown-subcommand check
// cobra applies only to the root, and routes any failure through
// reportArgsError.
func reportingArgs(cmd *cobra.Command) cobra.PositionalArgs {
	orig := cmd.Args
	return func(c *cobra.Command, args []string) error {
		if err := validateArgs(c, orig, args); err != nil {
			return reportArgsError(c, err)
		}
		return nil
	}
}

// validateArgs runs the structural checks in the order cobra would,
// plus the nested-group unknown-subcommand check cobra omits.
//
// The group check only fires for a command that HAS subcommands. No
// weave group takes positional args of its own (`weave`, `weave baton`,
// `weave fleet` all ignore them), so on a group ANY leftover positional
// is a name cobra's Find() failed to resolve — i.e. a typo'd subverb.
func validateArgs(c *cobra.Command, orig cobra.PositionalArgs, args []string) error {
	if c.HasSubCommands() && len(args) > 0 {
		return fmt.Errorf("unknown command %q for %q%s", args[0], c.CommandPath(), suggestSubcommands(c, args[0]))
	}
	if orig == nil {
		// Matches cobra's own default (ValidateArgs falls back to
		// ArbitraryArgs when Args is nil) for a command with no subverbs.
		return nil
	}
	return orig(c, args)
}

// suggestSubcommands renders cobra's own did-you-mean list for a
// mistyped subverb, in the parenthetical shape cobra uses on the root,
// or "" when nothing is close enough.
func suggestSubcommands(c *cobra.Command, typed string) string {
	// cobra initializes the suggestion edit-distance threshold only on
	// the ROOT command (ExecuteC defaults it to 2); on a nested group it
	// is still 0, which disables SuggestionsFor entirely. Mirror the
	// root default so `weave baton wrote` suggests `write`.
	if c.SuggestionsMinimumDistance <= 0 {
		c.SuggestionsMinimumDistance = 2
	}
	names := c.SuggestionsFor(typed)
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("; did you mean %s?", strings.Join(quoteAll(names), " or "))
}

func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf("%q", n))
	}
	return out
}

// reportArgsError emits the failure on the command's own stderr in the
// caller's requested output mode and returns a structured weavecli exit
// carrying ExitInvalidArg — the same contract weaveFlagErrorFunc uses.
func reportArgsError(cmd *cobra.Command, err error) error {
	msg := fmt.Sprintf("%s (run `%s --help` for usage)", err.Error(), cmd.CommandPath())
	return ec(weavecli.EmitError(cmd.ErrOrStderr(), flagErrOutputMode(cmd), cmd.CommandPath(),
		weavecli.ExitInvalidArg, fmt.Errorf("%s", msg)))
}
