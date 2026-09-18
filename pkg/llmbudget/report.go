package llmbudget

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

func CollectReport(ctx context.Context, opt ReportOptions) (*Report, error) {
	return defaultGate.CollectReport(ctx, opt)
}
func (g *Gate) CollectReport(ctx context.Context, opt ReportOptions) (*Report, error) {
	resolveModel, listModels := reportModelResolver(g)
	p, err := g.policy()
	if err != nil {
		return nil, err
	}
	s, err := g.stateSnapshot()
	if err != nil {
		return nil, err
	}
	if p.missing && s.HardPolicy {
		return nil, errors.New("llmbudget: prior hard policy missing")
	}
	now := opt.Now
	if now.IsZero() {
		now = g.now()
	}
	report := &Report{SchemaVersion: ReportSchemaVersion, GeneratedAt: now, Accounts: []AccountReport{}, Warnings: []string{}}
	roster := append([]Binding(nil), opt.Roster...)
	if opt.Roster == nil {
		if g.cfg.Models != nil {
			for name, m := range g.cfg.Models {
				roster = append(roster, Binding{Model: name, Provider: m.Provider, Lane: laneFor(m)})
			}
		} else {
			cat := fleet.New()
			models, errs := listModels()
			for _, e := range errs {
				report.Warnings = append(report.Warnings, e.Error())
			}
			for _, m := range models {
				roster = append(roster, Binding{Model: m.Name, Provider: m.Provider, Lane: laneFor(FromFleetModel(m))})
			}
			agents, errs := cat.Agents()
			for _, e := range errs {
				report.Warnings = append(report.Warnings, e.Error())
			}
			for _, a := range agents {
				roster = append(roster, Binding{Model: a.Model, Agent: a.Name})
			}
		}
	}
	for _, b := range p.Bindings {
		roster = append(roster, b)
	}
	for name := range s.Models {
		roster = append(roster, Binding{Model: name})
	}
	for _, a := range opt.Active {
		roster = append(roster, Binding{Model: a.Model, Agent: a.Agent})
	}
	for _, src := range p.Sources {
		roster = append(roster, Binding{Provider: src.Provider, Account: src.Account, Pool: src.Pool, Lane: src.Lane, AccountKnown: true})
	}
	groups := map[string]*AccountReport{}
	modelBindings := map[string][]Binding{}
	for _, b := range roster {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if m, ok := resolveModel(b.Model); ok {
			b.Model = m.Name
			if b.Provider == "" {
				b.Provider = m.Provider
			}
			if b.Lane == "" {
				b.Lane = laneFor(m)
			}
		}
		if explicit, ok := bindingFor(p, b.Model, b.Agent); ok {
			explicit.Agent = b.Agent
			b = explicit
		}
		if b.Provider == "" {
			b.Provider = "unknown"
		}
		if b.Lane == "" {
			b.Lane = LaneAPIKey
		}
		if b.Account == "" {
			b.Account = "unresolved"
			b.AccountKnown = false
		}
		if b.Pool == "" {
			b.Pool = "unresolved"
		}
		key := poolKey(b)
		row := groups[key]
		if row == nil {
			row = &AccountReport{Provider: b.Provider, Account: b.Account, Pool: b.Pool, Lane: b.Lane, AccountKnown: b.AccountKnown, Status: "unsupported", Models: []string{}, Agents: []string{}, Attribution: []Attribution{}, Metrics: []Metric{}, Limitations: []string{"Local observations do not cover all external account/host consumption."}}
			groups[key] = row
		}
		if !b.AccountKnown {
			row.AccountKnown = false
			row.Limitations = unique(row.Limitations, "Account identity is unresolved; grouped display does not establish a shared quota pool.")
		}
		if b.Model != "" {
			row.Models = unique(row.Models, b.Model)
			modelBindings[b.Model] = append(modelBindings[b.Model], b)
		}
		if b.Agent != "" {
			row.Agents = unique(row.Agents, b.Agent)
		}
	}
	for _, a := range opt.Active {
		if m, ok := resolveModel(a.Model); ok {
			a.Model = m.Name
		}
		for _, b := range modelBindings[a.Model] {
			if b.Agent != "" && a.Agent != "" && b.Agent != a.Agent {
				continue
			}
			row := groups[poolKey(b)]
			if row != nil && !hasAttribution(row.Attribution, a) {
				row.Attribution = append(row.Attribution, a)
			}
		}
	}
	for key, row := range groups {
		if opt.Provider != "" && !strings.EqualFold(opt.Provider, row.Provider) {
			continue
		}
		if opt.Model != "" {
			wanted := opt.Model
			if m, ok := resolveModel(wanted); ok {
				wanted = m.Name
			}
			if !contains(row.Models, wanted) {
				continue
			}
		}
		b := Binding{Provider: row.Provider, Account: row.Account, Pool: row.Pool, Lane: row.Lane}
		var dayTokens, weekTokens, requests int64
		var cost float64
		hasLocal := false
		unknownSpend := false
		unknownTokens := false
		legacy := false
		if pool, ok := s.Pools[key]; ok {
			cc := currentCounters(pool.Counters, now)
			dayTokens, weekTokens, requests, cost = cc.DayTokens, cc.WeekTokens, cc.DayRequests, cc.DayCostUSD
			hasLocal = true
			unknownTokens = pool.UnknownTokens && pool.UnknownTokensAt != nil && dayStart(*pool.UnknownTokensAt).Equal(dayStart(now))
			unknownSpend = pool.UnknownSpend && dayStart(pool.UnknownSpendAt).Equal(dayStart(now))
			for _, v := range []struct {
				name string
				n    int64
			}{{"usage.input_tokens", pool.InputTokens}, {"usage.output_tokens", pool.OutputTokens}, {"usage.cached_input_tokens", pool.CachedInputTokens}} {
				class := "actual"
				if pool.EstimatedTokens {
					class = "estimated"
				}
				row.Metrics = append(row.Metrics, measured(v.name, float64(v.n), "tokens", class, "local-meter", pool.ObservedAt))
			}
		}
		for _, model := range row.Models {
			if cc, ok := s.Unattributed[model]; ok {
				cc = currentCounters(cc, now)
				dayTokens = safeAdd(dayTokens, cc.DayTokens)
				weekTokens = safeAdd(weekTokens, cc.WeekTokens)
				requests = safeAdd(requests, cc.DayRequests)
				cost += cc.DayCostUSD
				hasLocal = true
				legacy = true
			}
		}
		if hasLocal {
			row.Status = "partial"
			start, end := dayStart(now), dayStart(now).AddDate(0, 0, 1)
			for _, v := range []struct {
				name, unit, class string
				n                 float64
			}{{"usage.tokens", "tokens", "actual", float64(dayTokens)}, {"usage.requests", "requests", "actual", float64(requests)}, {"billing.spend", "usd", "estimated", cost}} {
				m := measured(v.name, v.n, v.unit, v.class, "local-meter", now)
				if v.name == "usage.tokens" && unknownTokens {
					m.Value = nil
					m.Classification = "unknown"
					m.Limitation = "Some completed work has unknown token use; hard token admission retains that uncertainty."
				}
				if v.name == "billing.spend" && unknownSpend {
					m.Value = nil
					m.Classification = "unknown"
					m.Limitation = "Some completed work has no actual spend; conservative accounting is retained for admission."
				}
				m.WindowStart = &start
				m.WindowEnd = &end
				row.Metrics = append(row.Metrics, m)
			}
			_ = weekTokens
		}
		if legacy {
			row.Limitations = unique(row.Limitations, "Legacy model totals lack account/category attribution; shown conservatively and may overlap account rows.")
		}
		reservedTokens, reservedSpend := int64(0), int64(0)
		reservedTokensUnknown, reservedSpendUnknown := false, false
		for _, r := range s.Reservations {
			if poolKey(requestBinding(r.Request)) == key {
				row.ActiveReservations++
				reservedTokensUnknown = reservedTokensUnknown || r.Request.UnknownTokens
				reservedSpendUnknown = reservedSpendUnknown || (r.Request.SpendMicroUSD == nil && (r.Request.Tokens > 0 || r.Request.UnknownTokens))
				reservedTokens = safeAdd(reservedTokens, r.Request.Tokens)
				if r.Request.SpendMicroUSD != nil {
					reservedSpend = safeAdd(reservedSpend, *r.Request.SpendMicroUSD)
				}
				if !now.Before(r.ExpiresAt) {
					row.Limitations = unique(row.Limitations, "Expired renewal remains reserved pending verified work termination.")
				}
			}
		}
		row.Metrics = append(row.Metrics, measured("budget.reserved_tokens", float64(reservedTokens), "tokens", "actual", "local-reservations", now), measured("budget.reserved_spend", float64(reservedSpend)/1e6, "usd", "estimated", "local-reservations", now))
		if reservedTokensUnknown {
			i := len(row.Metrics) - 2
			row.Metrics[i].Value = nil
			row.Metrics[i].Classification = "unknown"
			row.Metrics[i].Limitation = "An active operation has unknown token demand."
		}
		if reservedSpendUnknown {
			i := len(row.Metrics) - 1
			row.Metrics[i].Value = nil
			row.Metrics[i].Classification = "unknown"
			row.Metrics[i].Limitation = "An active operation has unknown spend demand."
		}
		for _, c := range p.Constraints {
			if c.Host != "" || !matches(c, b, "") {
				continue
			}
			row.Metrics = append(row.Metrics, constraintMetrics(c, now)...)
		}
		for _, model := range row.Models {
			if m, ok := resolveModel(model); ok {
				row.Metrics = append(row.Metrics, legacyMetrics(m, now)...)
			}
		}
		for _, src := range p.Sources {
			if src.Provider != row.Provider || src.Account != row.Account || src.Pool != row.Pool || src.Lane != row.Lane {
				continue
			}
			result, e := g.source(ctx, src, now, opt.Refresh)
			if e != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("source %s: observation unavailable", src.ID))
			}
			row.Status = result.Status
			if row.Status == "" {
				row.Status = "unavailable"
			}
			row.Metrics = append(row.Metrics, result.Metrics...)
			row.Limitations = append(row.Limitations, result.Limitations...)
			row.RetryAt = result.RetryAt
		}
		for _, spec := range requiredMetrics {
			found := false
			for _, m := range row.Metrics {
				if m.Name == spec[0] {
					found = true
					break
				}
			}
			if !found {
				row.Metrics = append(row.Metrics, Metric{Name: spec[0], Unit: spec[1], Classification: "unknown", Source: "unavailable", ObservedAt: now, Limitation: "No supported observation/configured ceiling for this account."})
			}
		}
		sort.Strings(row.Models)
		sort.Strings(row.Agents)
		sort.SliceStable(row.Metrics, func(i, j int) bool { return row.Metrics[i].Name < row.Metrics[j].Name })
		report.Accounts = append(report.Accounts, *row)
	}
	sort.Slice(report.Accounts, func(i, j int) bool {
		a, b := report.Accounts[i], report.Accounts[j]
		return a.Provider+"/"+a.Account+"/"+a.Pool+string(a.Lane) < b.Provider+"/"+b.Account+"/"+b.Pool+string(b.Lane)
	})
	return report, nil
}

var requiredMetrics = [][2]string{{"usage.input_tokens", "tokens"}, {"usage.output_tokens", "tokens"}, {"usage.cached_input_tokens", "tokens"}, {"usage.requests", "requests"}, {"usage.requests_per_minute", "requests/minute"}, {"usage.input_tokens_per_minute", "tokens/minute"}, {"usage.output_tokens_per_minute", "tokens/minute"}, {"usage.context_remaining_percent", "percent"}, {"quota.remaining", "unknown"}, {"quota.limit", "unknown"}, {"quota.used_percent", "percent"}, {"limits.requests_per_minute", "requests/minute"}, {"limits.tokens_per_minute", "tokens/minute"}, {"limits.concurrent", "requests"}, {"billing.spend", "usd"}, {"budget.daily_tokens", "tokens"}, {"budget.daily_spend", "usd"}, {"budget.concurrent", "requests"}}

func measured(name string, value float64, unit, class, source string, at time.Time) Metric {
	return Metric{Name: name, Value: &value, Unit: unit, Classification: class, Source: source, ObservedAt: at}
}
func constraintMetrics(c Constraint, now time.Time) []Metric {
	var out []Metric
	for _, v := range []struct {
		name, unit string
		v          *int64
		scale      float64
	}{{"budget.daily_tokens", "tokens", c.DailyTokens, 1}, {"budget.weekly_tokens", "tokens", c.WeeklyTokens, 1}, {"budget.daily_spend", "usd", c.DailySpendMicroUSD, 1e6}, {"budget.weekly_spend", "usd", c.WeeklySpendMicroUSD, 1e6}} {
		if v.v != nil {
			m := measured(v.name, float64(*v.v)/v.scale, v.unit, "actual", "configured-policy", now)
			if c.Soft {
				m.Limitation = "Soft warning policy; not a vendor limit or admission refusal."
			}
			out = append(out, m)
		}
	}
	if c.Concurrency != nil {
		out = append(out, measured("budget.concurrent", float64(*c.Concurrency), "requests", "actual", "configured-policy", now))
	}
	return out
}
func unique(s []string, v string) []string {
	if !contains(s, v) {
		return append(s, v)
	}
	return s
}
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
func hasAttribution(s []Attribution, a Attribution) bool {
	for _, x := range s {
		if x == a {
			return true
		}
	}
	return false
}

func legacyMetrics(m Model, now time.Time) []Metric {
	var out []Metric
	add := func(name, unit string, v float64) {
		if v > 0 {
			metric := measured(name, v, unit, "actual", "configured-model:"+m.Name, now)
			metric.Limitation = "Legacy configured policy; scope follows the model/provider/plan metadata, not a vendor-reported ceiling."
			out = append(out, metric)
		}
	}
	add("budget.daily_spend", "usd", m.Limits.BudgetUSD)
	add("budget.provider_daily_spend", "usd", firstPositive(m.Limits.ProviderQuotaUSD, m.Limits.ProviderUSD))
	add("budget.daily_tokens", "tokens", float64(m.Limits.DailyTokens))
	add("budget.weekly_tokens", "tokens", float64(m.Limits.WeeklyTokens))
	add("budget.daily_requests", "requests", float64(m.Limits.DailyRequests))
	add("budget.weekly_requests", "requests", float64(m.Limits.WeeklyRequests))
	if m.Limits.RatePer > 0 {
		add("limits.tokens_per_minute", "tokens/minute", float64(m.Limits.RateTokens)*float64(time.Minute)/float64(m.Limits.RatePer))
	}
	return out
}

// A report sees one coherent catalog projection. Resolve canonical names and
// aliases from that projection instead of reparsing every YAML file per row.
// The cache is request-local: the next report sees edits/removals and new env
// metadata without a TTL or process-global stale/negative cache.
func reportModelResolver(g *Gate) (func(string) (Model, bool), func() ([]fleet.Model, []error)) {
	var models []fleet.Model
	var errs []error
	loaded := false
	catalog := map[string]Model{}
	list := func() ([]fleet.Model, []error) {
		if !loaded {
			loaded = true
			models, errs = fleet.New().Models()
			for _, fm := range models {
				m := FromFleetModel(fm)
				for _, name := range fm.Names() {
					if _, exists := catalog[name]; !exists {
						catalog[name] = m
					}
				}
			}
		}
		return models, errs
	}
	resolve := func(name string) (Model, bool) {
		if name == "" {
			return Model{}, false
		}
		if m, ok := g.cfg.Models[name]; ok {
			m.Name = nonEmpty(m.Name, name)
			return m, true
		}
		list()
		m, ok := catalog[name]
		return m, ok
	}
	return resolve, list
}
