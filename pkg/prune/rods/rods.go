// Package rods assembles yoke-owned prune nouns for the embedding shell.
// A bashy mount may register additional host/satellite nouns on this registry.
package rods

import (
	"github.com/qiangli/yoke/pkg/binmgr"
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
