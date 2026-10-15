package llmbudget

import (
	"context"
	"math"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

type headroomProbeContextKey struct{}

// SubscriptionHeadroom reports the measured remaining fraction of the vendor
// subscription window that owns binding. It is deliberately separate from
// Preview: observations rank seats, while Preview remains the admission gate.
//
// A value is known only when an enabled account source produced a measured
// quota.used_percent measurement. Configured limits, local counters, stale
// observations, and unresolved accounts do not become vendor quota facts.
func SubscriptionHeadroom(ctx context.Context, binding Binding) (remaining float64, known bool, err error) {
	return defaultGate.SubscriptionHeadroom(ctx, binding)
}

// MeasuredHeadroom resolves a policy binding and returns its observed remaining
// subscription quota.  Unlike a configured limit, a true result means an
// enabled provider source supplied an actual quota measurement.  Capability
// routing uses this to refuse an unmeasured seat rather than inventing capacity.
func MeasuredHeadroom(ctx context.Context, model, agent string) (remaining float64, binding Binding, known bool, err error) {
	return defaultGate.MeasuredHeadroom(ctx, model, agent)
}

// MeasuredHeadroom is the Gate-scoped form used by embedders and tests.
func (g *Gate) MeasuredHeadroom(ctx context.Context, model, agent string) (remaining float64, binding Binding, known bool, err error) {
	p, err := g.policy()
	if err != nil {
		return 0, Binding{}, false, err
	}
	binding, ok := bindingFor(p, model, agent)
	if !ok || binding.Lane != LaneSubscription {
		return 0, binding, false, nil
	}
	remaining, known, err = g.SubscriptionHeadroom(ctx, binding)
	return remaining, binding, known, err
}

// SubscriptionHeadroom is the Gate-scoped form used by tests and embedders.
func (g *Gate) SubscriptionHeadroom(ctx context.Context, binding Binding) (remaining float64, known bool, err error) {
	report, err := g.CollectReport(context.WithValue(ctx, headroomProbeContextKey{}, true), ReportOptions{Roster: []Binding{binding}})
	if err != nil {
		return 0, false, err
	}
	planName := ""
	if binding.Model != "" {
		if m, ok := fleet.New().Model(binding.Model); ok {
			planName = m.Plan
		}
	}
	remaining = 1
	for _, account := range report.Accounts {
		planProbe := false
		if planName != "" && planName == account.Account {
			for _, metric := range account.Metrics {
				if strings.HasPrefix(metric.Source, "probe:plan:") {
					planProbe = true
					break
				}
			}
		}
		if account.Lane != LaneSubscription || !account.AccountKnown ||
			(binding.Provider != "" && !strings.EqualFold(account.Provider, binding.Provider)) ||
			(!planProbe && binding.Account != "" && account.Account != binding.Account) ||
			(!planProbe && binding.Pool != "" && account.Pool != binding.Pool) {
			continue
		}
		if !planProbe && binding.Agent != "" && !contains(account.Agents, binding.Agent) {
			continue
		}
		if !planProbe && binding.Agent == "" && binding.Model != "" && !contains(account.Models, binding.Model) {
			continue
		}
		for _, metric := range account.Metrics {
			if metric.Name != "quota.used_percent" || metric.Value == nil || (metric.Classification != "actual" && metric.Classification != "measured") || metric.Source == "unavailable" {
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
