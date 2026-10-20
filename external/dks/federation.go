// Package dks is the `bashy dks federation` front-door for the dhnt
// federation plane (Karmada, execution tier 5): joining, leaving, and
// inspecting multi-cluster federation, plus the managed karmadactl CLI.
//
// It lives OUTSIDE the engine tree on purpose: the VM-provisioning verbs
// (`bashy dks provision|status`) need the in-process podman machine libraries
// and only exist in the bashy_engines build, but federation must be reachable
// on the released lean build too (Linux native-podman planes, Windows). The
// full build mounts NewFederationCmd as a subcommand of the engine's dks verb;
// the lean build routes `dks federation ...` here before its engine refusal.
//
// Like `bashy sphere` (the tier-4 pattern) this front-door has ZERO build
// dependency on outpost: federation membership is owned by the outpost
// cluster agent, resolved + exec'd at runtime via external/meshagent. The
// karmadactl CLI itself is a tier-5 declarative-registry entry
// (external/registry), downloaded + exec'd, never bundled.
package dks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/kube"
	"github.com/qiangli/yoke/external/meshagent"
	"github.com/qiangli/yoke/external/registry"
)

// ExecOutpost runs `outpost <args…>`. A variable (not a direct meshagent
// call) so tests — including the lean-build dispatch test in bashy — can stub
// the agent without installing one.
var ExecOutpost = meshagent.Exec

// execKarmadactl is replaceable by unit tests so they can verify the
// environment passed to the managed CLI without downloading it.
var execKarmadactl = runKarmadactl

// errHandled signals exit-non-zero without a re-printed error (message already
// emitted). SilenceErrors keeps cobra from prepending "Error:".
var errHandled = errors.New("dks federation: handled")

// federationVerbs are the `outpost cluster federation` subcommands this
// front-door passes through to. `bashy dks federation <v> …` maps straight to
// `outpost cluster federation <v> …`.
var federationVerbs = map[string]string{
	"join":   "join this node's cluster to the federation",
	"leave":  "remove this node's cluster from the federation",
	"status": "show federation membership and member-cluster health",
	"on":     "enable the federation control plane (passthrough)",
	"off":    "disable the federation control plane (passthrough)",
	"token":  "manage federation join tokens (passthrough)",
}

// IsFederation reports whether `bashy dks` args select the federation
// front-door (as opposed to the engine-only VM verbs). Dispatchers in every
// build consult this before the engine refusal so federation stays reachable
// where the provisioner is not.
func IsFederation(args []string) bool {
	return len(args) > 0 && args[0] == "federation"
}

// Dispatch runs `dks federation …` (args WITHOUT the leading "federation")
// and returns the process exit code. It is the shared entry for the lean
// build's early dispatch and any other non-cobra caller.
func Dispatch(args []string) int {
	cmd := NewFederationCmd()
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		if errors.Is(err, errHandled) {
			return 1
		}
		fmt.Fprintf(os.Stderr, "bashy dks federation: %v\n", err)
		return 1
	}
	return 0
}

// NewFederationCmd builds the `bashy dks federation` subcommand: a thin
// front-door over `outpost cluster federation …` with `karmadactl …`
// reaching the managed registry CLI. Selecting this command selects
// DKS_PROFILE=federation for its kubectl calls.
func NewFederationCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "federation",
		Short: "Join, leave, and inspect Karmada multi-cluster federation (via the outpost cluster agent)",
		Long: `federation is the dhnt federation plane (execution tier 5): the Karmada
control plane that binds peer DKS clusters without merging their identities.
Membership is owned by the outpost cluster agent; this is a thin front-door
that execs it, so bashy stays standalone (no build dependency on outpost).

Subcommands (pass straight through to 'outpost cluster federation'):
  bashy dks federation join    …    join this node's cluster to the federation
  bashy dks federation leave   …    remove this node's cluster from the federation
  bashy dks federation status  …    membership and member-cluster health
  bashy dks federation on|off|token …  control-plane and token passthrough

  bashy dks federation karmadactl …  the managed Karmada CLI (pinned release,
                                     sha256-verified; $KARMADACTL_VERSION pins it)

Selecting federation selects DKS_PROFILE=federation for the kubectl calls this
makes (kubeconfig $DKS_FEDERATION_KUBECONFIG, default
~/.kube/outpost-federation/karmada.yaml, fail-closed when missing, no
refresh). Set $OUTPOST_BIN to point at a specific outpost binary.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
				fmt.Fprint(cmd.OutOrStdout(), cmd.Long, "\n")
				return nil
			}
			sub := args[0]
			rest := args[1:]
			_, passthrough := federationVerbs[sub]
			if sub != "karmadactl" && !passthrough {
				fmt.Fprintf(cmd.ErrOrStderr(), "dks federation: unknown subcommand %q — try: join, leave, status, on, off, token, karmadactl\n", sub)
				return errHandled
			}
			// Select the federation plane only for this command. bashy can remain
			// alive after the child exits, so restore its previous profile.
			previous, hadPrevious := os.LookupEnv(kube.EnvProfile)
			if err := os.Setenv(kube.EnvProfile, kube.ProfileFederation); err != nil {
				return fmt.Errorf("select %s=%s: %w", kube.EnvProfile, kube.ProfileFederation, err)
			}
			defer func() {
				if hadPrevious {
					_ = os.Setenv(kube.EnvProfile, previous)
				} else {
					_ = os.Unsetenv(kube.EnvProfile)
				}
			}()
			if sub == "karmadactl" {
				return execKarmadactl(cmd.Context(), rest)
			}
			err := ExecOutpost(cmd.Context(), append([]string{"cluster", "federation", sub}, rest...)...)
			if errors.Is(err, meshagent.ErrNotFound) {
				fmt.Fprintln(cmd.ErrOrStderr(), joinLines)
				return errHandled
			}
			return err
		},
	}
}

// runKarmadactl reaches the managed karmadactl CLI through its
// tier-5 registry entry (download → sha256-verify → cache → exec). The
// DKS_PROFILE=federation selection above is already in the process env, so
// the provisioned binary inherits it like every other child here.
func runKarmadactl(ctx context.Context, args []string) error {
	e, ok := registry.Lookup("karmadactl")
	if !ok {
		return fmt.Errorf("karmadactl is not a registered managed CLI")
	}
	cmd := e.NewCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs(args)
	return cmd.Execute()
}

// joinLines is the clear message shown when outpost is absent: federation
// membership is driven by the outpost cluster agent, and without it there is
// nothing to federate.
var joinLines = strings.Join([]string{
	"dks federation: the outpost cluster agent isn't available on this machine.",
	"",
	"Federation membership is owned by outpost ('outpost cluster federation ...');",
	"this front-door only drives it. To federate this node's DKS cluster:",
	"",
	"    1. Install the outpost mesh agent (or put it on PATH)",
	"    2. Join: bashy dks federation join",
	"",
	"(Already have outpost installed? Put it on PATH or set $OUTPOST_BIN.)",
}, "\n")
