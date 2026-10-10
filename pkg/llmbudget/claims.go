package llmbudget

import (
	"context"
	"fmt"

	"github.com/qiangli/yoke/pkg/policy/coord"
)

// The gate is where every bashy-mediated LLM use passes (chat, weave admission,
// the cligw door, dag capacity), so it is where a coord claim on a model,
// provider, account, agent or host is enforced: a holder's own calls pass, any
// other agent is refused before a single unit of capacity is reserved.
//
// provider and account are the two axes only this package names; model, agent
// and host kinds come from the fleet providers. Guard works on whatever kind
// name is used — an unregistered kind is name-matched in its own domain.
func init() {
	coord.RegisterKind(coord.Kind{Name: "provider", Match: coord.MatchName})
	coord.RegisterKind(coord.Kind{Name: "account", Match: coord.MatchName})
}

// claimHolder is who is asking: the episode, so a claiming agent's own
// requests — from any of its processes — are admitted. Tests substitute it.
var claimHolder = coord.Self

// claimUses is what the (prepared) request touches. Each use carries its name
// as Member too, so a claim of a member-matched kind in the same domain (a pool
// whose Members list the model) blocks as well as a claim on the name itself.
func claimUses(r Request) []coord.Use {
	var uses []coord.Use
	for _, u := range [][2]string{{"model", r.Model}, {"provider", r.Provider}, {"account", r.Account}, {"agent", r.Agent}, {"host", r.Host}} {
		if u[1] != "" {
			uses = append(uses, coord.Use{Kind: u[0], Name: u[1], Member: u[1]})
		}
	}
	return uses
}

// guardClaims refuses r when a live claim held by someone else covers any of
// its uses. The error wraps the *coord.Conflict so callers can errors.As it
// and print its contact text.
func guardClaims(ctx context.Context, r Request) error {
	uses := claimUses(r)
	if len(uses) == 0 {
		return nil
	}
	if err := coord.Guard(ctx, claimHolder(), uses...); err != nil {
		return fmt.Errorf("llmbudget: %w", err)
	}
	return nil
}
