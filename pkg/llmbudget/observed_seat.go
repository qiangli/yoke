package llmbudget

import (
	"context"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// CachedObservedExhaustion reports only confirmed vendor exhaustion with a
// future reset. Seat selection does not trigger model analysis or a probe.
func CachedObservedExhaustion(ctx context.Context, model string, now time.Time) (string, time.Time, bool) {
	cat := fleet.New()
	m, ok := cat.Model(model)
	if !ok {
		return "", time.Time{}, false
	}
	account := quotaAccount(m)
	return observedExhaustion(account, Observed(ctx, now), now)
}

func observedExhaustion(account string, states []ObservedState, now time.Time) (string, time.Time, bool) {
	for _, state := range states {
		stateAccount := state.Account
		if stateAccount == "" {
			stateAccount = state.Plan
		}
		if stateAccount == account && state.Classification == "observed" && state.Kind == "exhausted" && state.ResetAt != nil && state.ResetAt.After(now) {
			return state.Signal, *state.ResetAt, true
		}
	}
	return "", time.Time{}, false
}
