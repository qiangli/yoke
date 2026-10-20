// userland.go — the `bashy dks` cluster-userland client verbs (contract §3):
// use, local, and the fail-closed placeholders for the verbs S481.3/S481.6
// will deliver. Like federation.go this front-door lives outside the engine
// tree so the lean build can route `bashy dks use|local|…` here before its
// engine refusal; the engine build mounts the same commands under `dks`.
//
// Zero ceremony on the default path: no YAML, no kubectl, no kubeconfig talk
// unless something fails and the fix needs naming. The fail-closed rule of
// the contract holds everywhere here: an unreachable plane, a missing
// workspace or an unavailable verb exits non-zero having run NOTHING.
package dks

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// userlandVerbs are the `bashy dks` client verbs of the cluster-userland
// contract §3. The spellings are final; logs and attach are registered but
// fail closed until S481.6 lands.
var userlandVerbs = map[string]string{
	"use":    "select the workspace later commands run in (create on first use)",
	"local":  "run one command on this machine, explicitly",
	"run":    "run a command as a scheduled workload in the selected workspace",
	"ps":     "list the workspace's runs and sessions",
	"logs":   "show a run's or session's output",
	"attach": "reattach a session from any paired client",
	"stop":   "stop exactly one run or session",
}

// unimplementedVerbs fail closed: registered for discovery and help, but they
// never run anything — locally or on a plane — until their story lands.
var unimplementedVerbs = []string{"logs", "attach"}

// notAvailableSprint names the stories that finish the unimplemented verbs.
const notAvailableSprint = "Sprint 481 S481.3/S481.6"

// errNotAvailable is the fail-closed sentinel of an unimplemented verb: the
// message is already on stderr; DispatchUserland maps it to exit code 2.
var errNotAvailable = errors.New("dks userland: not available yet")

// errUserlandHandled signals exit-non-zero with the message already emitted.
var errUserlandHandled = errors.New("dks userland: handled")

// IsUserland reports whether `bashy dks` args select a cluster-userland
// client verb (as opposed to the federation front-door or the engine-only VM
// verbs). Dispatchers in every build consult this before the engine refusal.
func IsUserland(args []string) bool {
	return len(args) > 0 && userlandVerbs[args[0]] != ""
}

// DispatchUserland runs `dks <userland verb> …` (args INCLUDING the verb) and
// returns the process exit code. It is the shared entry for the lean build's
// early dispatch and any other non-cobra caller, mirroring federation
// Dispatch. Fail-closed verbs exit 2; everything else that fails exits 1
// after printing its own actionable message.
func DispatchUserland(args []string) int {
	if !IsUserland(args) {
		fmt.Fprintf(os.Stderr, "bashy dks: %q is not a userland verb — try: use, local\n", firstArg(args))
		return 1
	}
	root := &cobra.Command{Use: "dks", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(NewUserlandCmds()...)
	root.SetArgs(args)
	return userlandExitCode(root.Execute())
}

// userlandExitCode maps a userland cobra error to the process exit code:
// errNotAvailable is the fail-closed 2, a local escape's exit status is the
// command's own, and everything else fails 1.
func userlandExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errNotAvailable):
		return 2
	default:
		var ec *exitStatusError
		if errors.As(err, &ec) {
			return ec.code
		}
		return 1
	}
}

// NewUserlandCmds builds the cluster-userland client commands of `bashy dks`.
// The engine build mounts them beside provision/status and federation; the
// lean build dispatches them early through DispatchUserland.
func NewUserlandCmds() []*cobra.Command {
	return newUserlandCmds(defaultUserlandEnv())
}

// newUserlandCmds wires the verbs onto one injectable environment so tests
// never touch the real state dir, plane or process tree.
func newUserlandCmds(env *userlandEnv) []*cobra.Command {
	cmds := []*cobra.Command{
		newUserlandUseCmd(env),
		newUserlandLocalCmd(env),
	}
	cmds = append(cmds, newUserlandRunCmds(env)...)
	for _, verb := range unimplementedVerbs {
		cmds = append(cmds, newNotAvailableCmd(verb))
	}
	return cmds
}

// newNotAvailableCmd registers an unimplemented verb that fails CLOSED: it
// prints the not-yet message and exits 2 without running anything. Flag
// parsing is disabled so future spellings (`run --detach sh -c …`) hit the
// closed door instead of a flag error.
func newNotAvailableCmd(verb string) *cobra.Command {
	return &cobra.Command{
		Use:   verb,
		Short: userlandVerbs[verb] + " (not available yet)",
		Long: verb + ": " + userlandVerbs[verb] + ".\n\nThis verb is not available yet (" + notAvailableSprint +
			"); it runs nothing — neither here nor on a plane. Meanwhile 'bashy dks use' selects a workspace and 'bashy dks local -- <cmd>' runs one command explicitly on this machine.",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
				fmt.Fprint(cmd.OutOrStdout(), cmd.Long, "\n")
				return nil
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks %s: not available yet (%s)\n", verb, notAvailableSprint)
			return errNotAvailable
		},
	}
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
