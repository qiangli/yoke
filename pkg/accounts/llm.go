package accounts

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/llmbudget"
)

// LLMAdapter exposes cached llmbudget quota, token usage, and cost as account
// metadata. Reads are cache-only: every collection passes Refresh=false, so
// the adapter never triggers probes or inference, and it never resolves or
// copies credential values.
type LLMAdapter struct {
	collect func(context.Context, llmbudget.ReportOptions) (*llmbudget.Report, error)
}

// NewLLMAdapter returns the LLM adapter over the default budget gate.
func NewLLMAdapter() *LLMAdapter {
	return &LLMAdapter{collect: llmbudget.CollectReport}
}

// The LLM adapter joins the default registry through the one adapter
// mechanism. Subpackage adapters join the same registry from builtin, so
// `account discover` sees every Discover-capable adapter by default.
func init() { MustRegisterAdapter(NewLLMAdapter()) }

func (a *LLMAdapter) Name() string { return "llm" }

func (a *LLMAdapter) Capabilities() Capability { return Discover | Verify | Usage | Billing }

func (a *LLMAdapter) report(ctx context.Context) (*llmbudget.Report, error) {
	collect := a.collect
	if collect == nil {
		collect = llmbudget.CollectReport
	}
	// Refresh=false: cached reads only. Never probe, never infer.
	return collect(ctx, llmbudget.ReportOptions{Refresh: false})
}

// seat groups the cached pool rows that share one verified provider identity.
// Pools and lanes stay distinguishable inside the seat (one connection per
// pool, lane recorded when uniform); separate provider identities never merge,
// even when they share a plan or contact address.
type seat struct {
	provider string
	account  string
	lanes    map[string]bool
	pools    map[string]llmbudget.Lane
	models   map[string]bool
	agents   map[string]bool
	rows     []llmbudget.AccountReport
}

func groupSeats(report *llmbudget.Report) map[string]*seat {
	seats := map[string]*seat{}
	if report == nil {
		return seats
	}
	for _, row := range report.Accounts {
		if !row.AccountKnown || row.Account == "" || row.Account == "unresolved" {
			continue
		}
		if row.Provider == "" || row.Provider == "unknown" || row.Provider == "none" {
			continue
		}
		if row.Account == "local" {
			continue
		}
		key := row.Provider + "\x00" + row.Account
		s := seats[key]
		if s == nil {
			s = &seat{provider: row.Provider, account: row.Account, lanes: map[string]bool{}, pools: map[string]llmbudget.Lane{}, models: map[string]bool{}, agents: map[string]bool{}}
			seats[key] = s
		}
		s.rows = append(s.rows, row)
		s.lanes[string(row.Lane)] = true
		if row.Pool != "" && row.Pool != "unresolved" {
			s.pools[row.Pool] = row.Lane
		}
		for _, m := range row.Models {
			s.models[m] = true
		}
		for _, ag := range row.Agents {
			s.agents[ag] = true
		}
	}
	return seats
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *seat) observation(at time.Time) Observation {
	a := Account{Category: LLM, Provider: s.provider, ProviderAccountID: s.account, Source: "llmbudget", Status: Resolved, ObservedAt: &at}
	if len(s.lanes) == 1 {
		for lane := range s.lanes {
			a.Lane = lane
		}
	}
	pools := make([]string, 0, len(s.pools))
	for p := range s.pools {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	for _, p := range pools {
		a.Connections = append(a.Connections, Connection{ID: p, Profile: p})
	}
	a.Links.Models = sortedKeys(s.models)
	a.Links.Agents = sortedKeys(s.agents)
	return Observation{Account: a}
}

func (a *LLMAdapter) Discover(ctx context.Context) ([]Observation, error) {
	report, err := a.report(ctx)
	if err != nil {
		return nil, err
	}
	now := report.GeneratedAt
	if now.IsZero() {
		now = time.Now()
	}
	var out []Observation
	for _, s := range groupSeats(report) {
		out = append(out, s.observation(now))
	}
	// Unverified identities stay unresolved: keyed by provider capacity pool,
	// never by plan, display name, or contact address.
	seen := map[string]bool{}
	for _, row := range report.Accounts {
		if row.AccountKnown || row.Provider == "" {
			continue
		}
		key := row.Provider + "\x00" + row.Pool + "\x00" + string(row.Lane)
		if seen[key] {
			continue
		}
		seen[key] = true
		at := now
		out = append(out, Observation{
			SourceKey: "llm:" + row.Provider + "/" + row.Pool + "/" + string(row.Lane),
			Account:   Account{Category: LLM, Provider: row.Provider, Source: "llmbudget", Links: Links{Models: append([]string(nil), row.Models...), Agents: append([]string(nil), row.Agents...)}, ObservedAt: &at},
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i].Account, out[j].Account
		if ai.Provider != aj.Provider {
			return ai.Provider < aj.Provider
		}
		if ai.ProviderAccountID != aj.ProviderAccountID {
			return ai.ProviderAccountID < aj.ProviderAccountID
		}
		return out[i].SourceKey < out[j].SourceKey
	})
	return out, nil
}

// Verify re-reads the cached report and returns the seat matching a
// verified (provider, account) identity. Unknown identities are not found,
// never merged.
func (a *LLMAdapter) Verify(ctx context.Context, acct Account) (Observation, error) {
	if acct.Provider == "" || acct.ProviderAccountID == "" {
		return Observation{}, ErrNotFound
	}
	report, err := a.report(ctx)
	if err != nil {
		return Observation{}, err
	}
	now := report.GeneratedAt
	if now.IsZero() {
		now = time.Now()
	}
	for _, s := range groupSeats(report) {
		if s.provider == acct.Provider && s.account == acct.ProviderAccountID {
			return s.observation(now), nil
		}
	}
	return Observation{}, ErrNotFound
}

func (a *LLMAdapter) Resources(context.Context, Account) ([]ResourceRef, error) {
	return nil, ErrUnsupported
}

// periodLength bounds day/week/month queries. Sources that cover less than
// the requested period keep their values but drop to partial coverage with
// an explicit limitation; a point sample never claims a full period.
func periodLength(period string) (time.Duration, error) {
	switch period {
	case "", "day":
		return 24 * time.Hour, nil
	case "week":
		return 7 * 24 * time.Hour, nil
	case "month":
		return 30 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("accounts: invalid usage period %q", period)
	}
}

func periodName(period string) string {
	if period == "" {
		return "day"
	}
	return period
}

// deadRow reports a cached row whose values must not be presented as current:
// missing, stale, or unsupported is nil, never zero.
func deadRow(status string) bool {
	switch status {
	case "unavailable", "unsupported", "not-configured", "needs-input", "stale":
		return true
	default:
		return false
	}
}

func appendLimitation(out []string, v string) []string {
	if v == "" {
		return out
	}
	for _, have := range out {
		if have == v {
			return out
		}
	}
	return append(out, v)
}

// convertMetric maps one cached budget metric onto the shared metric
// contract. Vendor measurements become provider-reported, local meter and
// reservation counters become local observations, and inferred or estimated
// values stay estimates with explicit limitations.
func convertMetric(lm llmbudget.Metric, row llmbudget.AccountReport, period time.Duration, periodLabel string, now time.Time) Metric {
	out := Metric{Name: lm.Name, Unit: lm.Unit, ObservedAt: lm.ObservedAt, WindowStart: lm.WindowStart, WindowEnd: lm.WindowEnd, Period: periodLabel, Coverage: Complete}
	if out.ObservedAt.IsZero() {
		out.ObservedAt = now
	}
	if out.Unit == "usd" {
		out.Currency = "USD"
	}
	out.Source = lm.Source
	if out.Source == "" {
		out.Source = "unavailable"
	}
	for _, l := range row.Limitations {
		out.Limitations = appendLimitation(out.Limitations, l)
	}
	out.Limitations = appendLimitation(out.Limitations, lm.Limitation)
	switch cls := lm.Classification; cls {
	case "estimated", "inferred":
		out.Classification = Estimate
	case "unknown":
		out.Classification = LocalObservation
	default:
		switch out.Source {
		case "local-meter", "local-reservations":
			out.Classification = LocalObservation
		case "configured-policy":
			out.Classification = Estimate
			out.Limitations = appendLimitation(out.Limitations, "Configured policy ceiling, not a vendor measurement.")
		case "unavailable":
			out.Classification = LocalObservation
		default:
			out.Classification = ProviderReported
		}
	}
	dead := lm.Value == nil || lm.Classification == "unknown" || lm.Classification == "stale" || out.Source == "unavailable" || deadRow(row.Status)
	if dead {
		out.Value = nil
		out.Coverage = Unknown
		if out.Classification == Estimate && len(out.Limitations) == 0 {
			out.Limitations = append(out.Limitations, "Estimated value; not a vendor measurement.")
		}
		return out
	}
	v := *lm.Value
	out.Value = &v
	if lm.ResetAt != nil {
		out.Limitations = appendLimitation(out.Limitations, "Window resets at "+lm.ResetAt.UTC().Format(time.RFC3339)+".")
	}
	covered := false
	if lm.WindowStart != nil && lm.WindowEnd != nil {
		if d := lm.WindowEnd.Sub(*lm.WindowStart); d >= period {
			covered = true
		}
	}
	if !covered {
		out.Coverage = Partial
		if lm.WindowStart != nil && lm.WindowEnd != nil {
			out.Limitations = appendLimitation(out.Limitations, "Source window covers "+lm.WindowEnd.Sub(*lm.WindowStart).String()+"; "+periodLabel+" coverage is partial.")
		} else {
			out.Limitations = appendLimitation(out.Limitations, "Point-in-time reading; "+periodLabel+" coverage is partial.")
		}
	}
	if out.Classification == Estimate && len(out.Limitations) == 0 {
		out.Limitations = append(out.Limitations, "Estimated value; not a vendor measurement.")
	}
	return out
}

func (a *LLMAdapter) accountMetrics(ctx context.Context, acct Account, period string, billing bool) ([]Metric, error) {
	need, err := periodLength(period)
	if err != nil {
		return nil, err
	}
	label := periodName(period)
	report, err := a.report(ctx)
	if err != nil {
		return nil, err
	}
	now := report.GeneratedAt
	if now.IsZero() {
		now = time.Now()
	}
	seats := groupSeats(report)
	var s *seat
	for _, cand := range seats {
		if cand.provider == acct.Provider && cand.account == acct.ProviderAccountID {
			s = cand
			break
		}
	}
	if s == nil {
		return nil, ErrNotFound
	}
	var out []Metric
	for _, row := range s.rows {
		for _, lm := range row.Metrics {
			isBilling := strings.HasPrefix(lm.Name, "billing.") || lm.Name == "usage.balance" || (strings.HasPrefix(lm.Name, "budget.") && (lm.Unit == "usd" || strings.Contains(lm.Name, "spend")))
			if isBilling != billing {
				continue
			}
			out = append(out, convertMetric(lm, row, need, label, now))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Source < out[j].Source
	})
	return out, nil
}

// Usage returns cached token, request, and quota metrics for one verified
// seat. Context gauges are included for inspection but SumMetrics refuses to
// accumulate them as cumulative usage.
func (a *LLMAdapter) Usage(ctx context.Context, acct Account, period string) ([]Metric, error) {
	return a.accountMetrics(ctx, acct, period, false)
}

// Billing returns cached spend and balance metrics for one verified seat.
func (a *LLMAdapter) Billing(ctx context.Context, acct Account, period string) ([]Metric, error) {
	return a.accountMetrics(ctx, acct, period, true)
}

// Select returns safe routing metadata only. The adapter holds no
// credentials and never returns credential references.
func (a *LLMAdapter) Select(_ context.Context, acct Account, _ Operation) (Selection, error) {
	if acct.ID == "" {
		return Selection{}, ErrNotFound
	}
	return Selection{AccountID: acct.ID, Username: acct.Username}, nil
}
