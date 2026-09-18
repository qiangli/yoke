package llmbudget

import "sort"

// Status is the read side of the meter: it answers "what have I spent, what is
// left" so an agent can summarise early, drop a verification round, or hand off
// — instead of being cut off mid-thought by a gate it could not see.
//
// UNKNOWN IS NEVER ZERO AND NEVER INFINITY. When no ceiling is known, Limit and
// Remaining are nil and LimitKnown is false. A nil pointer cannot be read as 0
// ("exhausted") the way a zero-valued int64 can, and it cannot be read as
// unlimited either — the caller has to branch on LimitKnown. The spelling
// follows the pricing_known label the cost metric already carries; an absent
// limit reported as "unlimited" is the same class of bug as an absent test
// result reported as a pass.
type BudgetStatus struct {
	Error      string `json:"error,omitempty"`
	Model      string `json:"model"`
	ModelKnown bool   `json:"model_known"`
	Lane       Lane   `json:"lane,omitempty"`
	Plan       string `json:"plan,omitempty"`

	// Basis names the ceiling these numbers are measured against — the
	// binding one, i.e. the ceiling with the least headroom. Unit is the
	// unit Spent/Limit/Remaining are counted in: "tokens", "requests", or
	// "micro_usd". Spent is a local fact and is always known.
	Basis string `json:"basis"`
	Unit  string `json:"unit"`
	Spent int64  `json:"spent"`

	LimitKnown bool   `json:"limit_known"`
	Limit      *int64 `json:"limit,omitempty"`
	Remaining  *int64 `json:"remaining,omitempty"`
}

// Units for BudgetStatus.Unit.
const (
	UnitTokens   = "tokens"
	UnitRequests = "requests"
	UnitMicroUSD = "micro_usd"
)

// Status reports the default gate's current reading for a model. It is
// READ-ONLY: it mutates no counter, triggers no save, and fires no bound event.
// A read that costs you budget is not a read.
func Status(model string) BudgetStatus { return defaultGate.Status(model) }

// StatusAll reports one reading per model this host has actually recorded
// usage for, in stable name order.
func StatusAll() []BudgetStatus { return defaultGate.StatusAll() }

// Status is the per-gate accessor behind the package-level Status.
func (g *Gate) Status(model string) BudgetStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stateErr = g.reload()
	return g.status(model)
}

// StatusAll is the per-gate accessor behind the package-level StatusAll.
func (g *Gate) StatusAll() []BudgetStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stateErr = g.reload()
	names := make([]string, 0, len(g.state.Models))
	for name := range g.state.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]BudgetStatus, 0, len(names))
	for _, name := range names {
		out = append(out, g.status(name))
	}
	return out
}

// status computes one reading. Caller holds g.mu and has called ensureLoaded.
func (g *Gate) status(model string) BudgetStatus {
	now := g.now()
	m, known := g.model(model)
	s := BudgetStatus{Model: model, ModelKnown: known}
	if g.stateErr != nil {
		s.Error = g.stateErr.Error()
		s.Basis = "unavailable"
		return s
	}
	if !known {
		// No metadata at all: the counters are still real, the ceiling is not.
		c := currentCounters(g.state.Models[model], now)
		s.Basis, s.Unit, s.Spent = "day_tokens", UnitTokens, c.DayTokens
		return s
	}
	s.Lane = laneFor(m)
	s.Plan = planName(m)

	type candidate struct {
		basis string
		unit  string
		spent int64
		limit int64 // <= 0 means "no ceiling known for this axis"
	}
	var cands []candidate

	switch s.Lane {
	case LaneAPIKey:
		// usdLimit keeps an unset (<=0) USD ceiling at 0 rather than letting
		// microUSD turn it into a positive-looking rounded value.
		usdLimit := func(v float64) int64 {
			if v <= 0 {
				return 0
			}
			return microUSD(v)
		}
		day := microUSD(g.totalDayCostUSD())
		prov := microUSD(currentCounters(g.state.Providers[m.Provider], now).DayCostUSD)
		cands = []candidate{
			{"api_key_budget", UnitMicroUSD, day, usdLimit(m.Limits.BudgetUSD)},
			{"api_key_provider_quota", UnitMicroUSD, prov, usdLimit(firstPositive(m.Limits.ProviderQuotaUSD, m.Limits.ProviderUSD))},
		}
	case LaneSubscription:
		c := currentCounters(g.state.Plans[s.Plan], now)
		cands = []candidate{
			{"subscription_daily_tokens", UnitTokens, c.DayTokens, m.Limits.DailyTokens},
			{"subscription_weekly_tokens", UnitTokens, c.WeekTokens, m.Limits.WeeklyTokens},
			{"subscription_daily_requests", UnitRequests, c.DayRequests, m.Limits.DailyRequests},
			{"subscription_weekly_requests", UnitRequests, c.WeekRequests, m.Limits.WeeklyRequests},
		}
	default:
		c := currentCounters(g.state.Models[model], now)
		cands = []candidate{{"day_tokens", UnitTokens, c.DayTokens, 0}}
	}

	// The binding ceiling is the one with the least headroom. Fall back to the
	// first candidate for Spent when no ceiling is known, so the local fact
	// survives even though the limit does not.
	best := -1
	for i, c := range cands {
		if c.limit <= 0 {
			continue
		}
		if best < 0 || c.limit-c.spent < cands[best].limit-cands[best].spent {
			best = i
		}
	}
	if best < 0 {
		s.Basis, s.Unit, s.Spent = cands[0].basis, cands[0].unit, cands[0].spent
		return s // LimitKnown false, Limit/Remaining nil — UNKNOWN, not 0, not ∞.
	}
	c := cands[best]
	limit, remaining := c.limit, c.limit-c.spent
	if remaining < 0 {
		remaining = 0
	}
	s.Basis, s.Unit, s.Spent = c.basis, c.unit, c.spent
	s.LimitKnown, s.Limit, s.Remaining = true, &limit, &remaining
	return s
}
