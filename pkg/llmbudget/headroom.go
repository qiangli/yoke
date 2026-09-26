package llmbudget

import (
	"context"
	"math"
	"strings"
)

// SubscriptionHeadroom reports the measured remaining fraction of the vendor
// subscription window that owns binding. It is deliberately separate from
// Preview: observations rank seats, while Preview remains the admission gate.
//
// A value is known only when an enabled account source produced an actual
// quota.used_percent measurement. Configured limits, local counters, stale
// observations, and unresolved accounts do not become vendor quota facts.
func SubscriptionHeadroom(ctx context.Context, binding Binding) (remaining float64, known bool, err error) {
	return defaultGate.SubscriptionHeadroom(ctx, binding)
}

// SubscriptionHeadroom is the Gate-scoped form used by tests and embedders.
func (g *Gate) SubscriptionHeadroom(ctx context.Context, binding Binding) (remaining float64, known bool, err error) {
	report, err := g.CollectReport(ctx, ReportOptions{Roster: []Binding{binding}})
	if err != nil {
		return 0, false, err
	}
	remaining = 1
	for _, account := range report.Accounts {
		if account.Lane != LaneSubscription || !account.AccountKnown ||
			(binding.Provider != "" && !strings.EqualFold(account.Provider, binding.Provider)) ||
			(binding.Account != "" && account.Account != binding.Account) ||
			(binding.Pool != "" && account.Pool != binding.Pool) {
			continue
		}
		if binding.Agent != "" && !contains(account.Agents, binding.Agent) {
			continue
		}
		if binding.Agent == "" && binding.Model != "" && !contains(account.Models, binding.Model) {
			continue
		}
		for _, metric := range account.Metrics {
			if metric.Name != "quota.used_percent" || metric.Value == nil || metric.Classification != "actual" || metric.Source == "unavailable" {
				continue
			}
			used := *metric.Value
			if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
				continue
			}
			fraction := 1 - min(used, 100)/100
			if !known || fraction < remaining {
				remaining = fraction
			}
			known = true
		}
	}
	if !known {
		return 0, false, nil
	}
	return remaining, true, nil
}
