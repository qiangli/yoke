// Package rods assembles yoke-owned prune nouns for the embedding shell.
// Bashy mounts these nouns into their existing command trees.
package rods

import (
	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/prune"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/weave"
	"github.com/spf13/cobra"
)

func Registry() *prune.Registry {
	r := prune.NewRegistry()
	for _, n := range []prune.Noun{
		weave.AgentPruner{}, coord.ClaimPruner{}, binmgr.SelfPruner{},
		src.Pruner{}, weave.SprintPruner{}, weave.WeavePruner{},
		&fleet.HostPruner{}, &fleet.SatellitePruner{},
	} {
		if err := r.Register(n); err != nil {
			panic(err) // fixed constructor list; duplicate is a programming error
		}
	}
	return r
}

func NewOverviewCommand() *cobra.Command { return prune.NewOverviewCommand(Registry()) }

func NewAgentPruneCommand() *cobra.Command { return prune.NewNounCommand(weave.AgentPruner{}) }

func NewSelfPruneCommand() *cobra.Command { return prune.NewNounCommand(binmgr.SelfPruner{}) }

func NewHostPruneCommand() *cobra.Command {
	n := &fleet.HostPruner{}
	c := prune.NewNounCommand(n)
	c.Flags().StringVar(&n.Server, "server", "", "Cloudbox server URL (defaults to paired server)")
	c.Flags().StringVar(&n.Token, "token", "", "owner Bearer token")
	return c
}

func NewSatellitePruneCommand() *cobra.Command {
	n := &fleet.SatellitePruner{}
	c := prune.NewNounCommand(n)
	c.Flags().StringVar(&n.Master, "master", "", "master Cloudbox URL")
	c.Flags().StringVar(&n.Token, "token", "", "owner Bearer token")
	return c
}

func NewSatelliteTransferCommand() *cobra.Command { return fleet.NewSatelliteTransferCmd() }
