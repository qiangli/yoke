// Package sphere is the `bashy sphere` front-door for the dhnt SPHERE tier
// (execution tier 4): multi-node, peer-direct pooled p2p inference/compute — the
// layer between a single-node sandbox and an orchestrated cluster.
//
// The sphere data plane (libp2p mesh, model sharding, LLM pool, peer discovery)
// is owned by the outpost mesh agent. bashy is the userland keystone and must
// stay standalone, so this front-door has ZERO build dependency on outpost: it
// resolves + execs the agent at runtime via external/meshagent. Without outpost
// there is no p2p sphere — reported clearly, with an inviting join path.
package sphere

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/meshagent"
)

// errHandled signals exit-non-zero without a re-printed error (message already
// emitted). SilenceErrors keeps cobra from prepending "Error:".
var errHandled = errors.New("sphere: handled")

// subVerbs are the outpost subcommands that make up the sphere tier. `bashy
// sphere <v> …` maps straight to `outpost <v> …`.
var subVerbs = map[string]string{
	"mesh":  "libp2p peer-to-peer transport (the data plane)",
	"shard": "intra-LAN model sharding over the mesh",
	"pool":  "this node's LLM-pool participation",
	"peers": "discovery cache, reachability, predictions",
}

// ResolveOutpost re-exports meshagent.Resolve so callers (e.g. `bashy doctor`)
// keep one import for sphere readiness.
func ResolveOutpost() (string, bool) { return meshagent.Resolve() }

// NewSphereCmd builds the `bashy sphere` command: a thin passthrough to the
// outpost mesh agent's sphere subcommands.
func NewSphereCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sphere",
		Short: "Peer-direct pooled p2p inference/compute — the sphere tier (via the outpost mesh agent)",
		Long: `sphere is dhnt execution tier 4: multi-node, PEER-DIRECT pooled inference and
compute over the libp2p mesh (no control plane — that is the cluster tier). The
data plane is the outpost mesh agent; this is a thin front-door that execs it, so
bashy stays standalone (no build dependency on outpost). Without outpost running
there is no p2p sphere.

Subcommands (pass straight through to outpost):
  bashy sphere mesh  …    libp2p peer-to-peer transport (the data plane)
  bashy sphere shard …    intra-LAN model sharding over the mesh
  bashy sphere pool  …    this node's LLM-pool participation
  bashy sphere peers …    discovery cache, reachability, predictions
  bashy sphere status     quick overview (= outpost peers status)

Set $OUTPOST_BIN to point at a specific outpost binary.`,
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
			if sub == "status" { // friendly alias for the most useful overview
				sub, rest = "peers", append([]string{"status"}, rest...)
			}
			if _, ok := subVerbs[sub]; !ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "sphere: unknown subcommand %q — try: mesh, shard, pool, peers, status\n", sub)
				return errHandled
			}
			err := meshagent.Exec(cmd.Context(), append([]string{sub}, rest...)...)
			if errors.Is(err, meshagent.ErrNotFound) {
				fmt.Fprintln(cmd.ErrOrStderr(), joinLines)
				return errHandled
			}
			return err
		},
	}
}

// joinLines is the clear, inviting message shown when this machine isn't part of
// a sphere yet: pool inference/compute across the computers you own by joining
// Tessaro (the front door), which pairs this machine via the outpost mesh agent.
var joinLines = strings.Join([]string{
	"sphere: this machine isn't part of a p2p sphere yet.",
	"",
	"The sphere tier pools inference + compute across the computers you already own,",
	"peer-to-peer. Join the mesh through Tessaro — the front door:",
	"",
	"    1. Sign in / sign up:   https://tessaro.sh",
	"    2. Add this machine     (installs the outpost mesh agent + pairs it)",
	"",
	"Then `bashy sphere mesh|shard|pool|peers` drives your mesh.",
	"(Already have outpost installed? Put it on PATH or set $OUTPOST_BIN.)",
}, "\n")
