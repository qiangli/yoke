package llmbudget

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

type QuotaRow struct {
	Account          string     `json:"account"`
	Label            string     `json:"label"`
	Limit            string     `json:"limit,omitempty"`
	Configured       bool       `json:"configured"`
	Provider         string     `json:"provider"`
	Plan             string     `json:"plan,omitempty"`
	Lane             Lane       `json:"lane,omitempty"`
	Window           string     `json:"window,omitempty"`
	UsedPercent      *float64   `json:"used_percent,omitempty"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
	Tokens           *float64   `json:"tokens,omitempty"`
	Balance          *float64   `json:"balance,omitempty"`
	BalanceUnit      string     `json:"balance_unit,omitempty"`
	Source           string     `json:"source,omitempty"`
	ObservedAt       *time.Time `json:"observed_at,omitempty"`
	Stale            bool       `json:"stale,omitempty"`
	Limitations      []string   `json:"limitations,omitempty"`
	Status           string     `json:"status"`
	Classification   string     `json:"classification,omitempty"`
	Headroom         *float64   `json:"headroom,omitempty"`
	SuggestedWorkers *int       `json:"suggested_workers,omitempty"`
	Predict          string     `json:"predict,omitempty"`
	ExhaustsAt       *time.Time `json:"exhausts_at,omitempty"`
}

// QuotaOptions.Refresh allows each probe its run.timeout (default 10s, cap 30s)
// despite a shorter caller deadline; explicit cancellation is still honored.
type QuotaOptions struct {
	All      bool
	Provider string
	Refresh  bool
	Now      time.Time
}

func Quota(ctx context.Context, opt QuotaOptions) ([]QuotaRow, error) {
	return defaultGate.Quota(ctx, opt)
}
func (g *Gate) Quota(ctx context.Context, opt QuotaOptions) ([]QuotaRow, error) {
	now := opt.Now
	if now.IsZero() {
		now = g.now()
	}
	report, err := g.CollectReport(ctx, ReportOptions{Refresh: opt.Refresh, Now: now})
	if err != nil {
		return nil, err
	}
	if opt.Refresh {
		if _, err := g.Analyze(ctx, AnalyzeOptions{Now: now}); err != nil {
			return nil, err
		}
	}
	cat := fleet.New()
	models, _ := cat.Models()
	plans, _ := cat.Plans()
	p, err := g.policy()
	if err != nil {
		return nil, err
	}
	agents, _ := cat.Agents()
	activeAgents := []fleet.Agent{}
	for _, agent := range agents {
		if cat.AgentActive(agent) {
			seen := map[string]bool{}
			for agent.Base != "" && !seen[agent.Name] {
				seen[agent.Name] = true
				agent, _ = cat.Agent(agent.Base)
			}
			if m, ok := cat.Model(agent.Model); ok {
				agent.Model = m.Name
			}
			activeAgents = append(activeAgents, agent)
		}
	}
	rows := quotaRows(report, models, plans, activeAgents, p, opt, now)
	applyForecast(rows, forecastHistory(now), now)
	return rows, nil
}

// quotaRows consumes active bindings, never model availability, as configuration.
func quotaRows(report *Report, models []fleet.Model, plans []fleet.Plan, agents []fleet.Agent, p *Policy, opt QuotaOptions, now time.Time) []QuotaRow {
	byPlan := map[string]fleet.Plan{}
	accounts := map[string]*QuotaRow{}
	accountModels := map[string][]fleet.Model{}
	tools := map[string]map[string]bool{}
	for _, plan := range plans {
		byPlan[plan.Name] = plan
		accounts[plan.Name] = &QuotaRow{Account: plan.Name, Plan: plan.Name, Provider: plan.Vendor, Lane: LaneSubscription}
	}
	modelAccounts := map[string]string{}
	for _, m := range models {
		id := quotaAccount(m)
		if id == "" {
			continue
		}
		modelAccounts[m.Name] = id
		accountModels[id] = append(accountModels[id], m)
		if row := accounts[id]; row != nil && len(accountModels[id]) == 1 {
			row.Provider = m.Provider
		}
		if accounts[id] == nil {
			accounts[id] = &QuotaRow{Account: id, Plan: m.Plan, Provider: m.Provider, Lane: laneFor(FromFleetModelPlan(m, fleet.Plan{}))}
		}
	}
	for _, agent := range agents {
		id := modelAccounts[agent.Model]
		if id == "" {
			continue
		}
		accounts[id].Configured = true
		if tools[id] == nil {
			tools[id] = map[string]bool{}
		}
		tools[id][agent.Tool] = true
	}
	for id, row := range accounts {
		row.Label = byPlan[row.Plan].Label
		if row.Label == "" && len(tools[id]) == 1 {
			for tool := range tools[id] {
				row.Label = tool
			}
		}
		if row.Label == "" {
			family := ""
			for i, m := range accountModels[id] {
				if i == 0 {
					family = m.Family
				} else if family != m.Family {
					family = ""
					break
				}
			}
			row.Label = family
		}
		if row.Label == "" {
			row.Label = id
		}
		if id == "local" {
			row.Label = "local"
		}
	}
	eligible := func(row *QuotaRow) bool {
		return row != nil && (row.Configured || opt.All) && (opt.Provider == "" || strings.EqualFold(row.Provider, opt.Provider) || strings.EqualFold(row.Account, opt.Provider) || strings.EqualFold(row.Label, opt.Provider))
	}
	rows := map[string]*QuotaRow{}
	tokenParts := map[string]float64{}
	hasParts := map[string]bool{}
	for _, account := range report.Accounts {
		id := account.Account
		if id == "" || id == "unresolved" || strings.EqualFold(account.Provider, "none") || strings.EqualFold(account.Provider, "unknown") {
			continue
		}
		// A model-level probe may name the key even when the model bills to a plan.
		if accounts[id] == nil && len(account.Models) > 0 {
			resolved := modelAccounts[account.Models[0]]
			for _, model := range account.Models[1:] {
				if modelAccounts[model] != resolved {
					resolved = ""
					break
				}
			}
			id = resolved
		}
		if account.Lane == LaneLocal {
			id = "local"
		}
		base := accounts[id]
		if !eligible(base) || !base.Configured {
			continue
		}
		if account.Status == "not-configured" {
			base.Status = account.Status
			base.Limitations = append(base.Limitations, account.Limitations...)
		}
		for _, metric := range account.Metrics {
			if metric.Value == nil || metric.Source == "unavailable" || metric.Source == "local-meter" {
				continue
			}
			if metric.Name != "quota.used_percent" && metric.Name != "usage.tokens_in" && metric.Name != "usage.tokens_out" && metric.Name != "usage.balance" && metric.Name != "usage.used" && metric.Name != "usage.limit" && metric.Name != "usage.remaining" {
				continue
			}
			window := metric.Window
			key := id + "\x00" + window
			row := rows[key]
			if row == nil {
				copy := *base
				row = &copy
				row.Window = window
				row.Status = "ok"
				if metric.Classification == "inferred" {
					row.Status = "inferred"
				}
				rows[key] = row
			}
			if row.Source == "" || metric.Name == "quota.used_percent" {
				row.Source = metric.Source
			}
			t := metric.ObservedAt
			if row.ObservedAt == nil || t.After(*row.ObservedAt) {
				row.ObservedAt = &t
			}
			if metric.ResetAt != nil {
				row.ResetAt = metric.ResetAt
			}
			row.Stale = row.Stale || metric.Classification == "stale"
			if row.Classification == "" || metric.Name == "quota.used_percent" {
				row.Classification = metric.Classification
			}
			switch metric.Name {
			case "quota.used_percent":
				if row.UsedPercent == nil || *metric.Value > *row.UsedPercent {
					row.UsedPercent = metric.Value
				}
				if metric.Classification == "measured" || metric.Classification == "actual" {
					h := 1 - math.Min(*metric.Value, 100)/100
					if h < 0 {
						h = 0
					}
					if row.Headroom == nil || h < *row.Headroom {
						row.Headroom = &h
					}
				}
			case "usage.tokens_in", "usage.tokens_out":
				tokenParts[key] += *metric.Value
				hasParts[key] = true
			case "usage.balance":
				row.Balance = metric.Value
				row.BalanceUnit = metric.Unit
			}
		}
	}
	for key, row := range rows {
		if hasParts[key] {
			v := tokenParts[key]
			row.Tokens = &v

		}
	}
	for _, r := range rows {
		if r.Headroom != nil {
			limit := 0
			limitKnown := false
			for _, m := range accountModels[r.Account] {
				if n, found := byPlan[m.Plan].Limit("concurrent", "requests", m.Name); found {
					limit = int(n)
					limitKnown = true
				}
			}
			if !limitKnown {
				for _, c := range p.Constraints {
					if (c.Provider == r.Provider || c.Provider == "") && (c.Account == "" || c.Account == r.Account) && c.Host == "" && c.Concurrency != nil {
						if !limitKnown || *c.Concurrency < limit {
							limit = *c.Concurrency
							limitKnown = true
						}
					}
				}
			}
			if limitKnown {
				n := int(math.Floor(float64(limit) * *r.Headroom))
				r.SuggestedWorkers = &n
			}
		}
	}
	// Local models and the door share one account; only active bindings configure it.
	if tokens, ok := doorTokens(now); ok {
		base := accounts["local"]
		if eligible(base) && base.Configured {
			copy := *base
			copy.Window, copy.Status, copy.Source, copy.Classification = "5h", "ok", "bashy-door", "measured"
			v := float64(tokens)
			copy.Tokens, copy.ObservedAt = &v, &now
			rows["local\x005h"] = &copy
		}
	}
	for id, base := range accounts {
		if !eligible(base) {
			continue
		}
		found := false
		for _, row := range rows {
			if row.Account == id {
				found = true
				break
			}
		}
		if found {
			continue
		}
		copy := *base
		if copy.Status == "" {
			copy.Status = "unmeasured"
		}
		if !base.Configured {
			copy.Status = "not-configured"
		}
		for _, limit := range byPlan[base.Plan].Limits {
			if limit.Value <= 0 {
				continue
			}
			if limit.Model != "" {
				matched := false
				for _, m := range accountModels[id] {
					if m.Name == limit.Model {
						matched = true
					}
				}
				if !matched {
					continue
				}
			}
			key := id + "\x00" + limit.Window
			row := rows[key]
			if row == nil {
				r := copy
				row = &r
				row.Window = limit.Window
				row.Source = "plan"
				rows[key] = row
			}
			if row.Limit != "" {
				row.Limit += "; "
			}
			row.Limit += fmt.Sprintf("%d %s/%s", limit.Value, limit.Unit, limit.Window)
			found = true
		}
		if !found {
			rows[id+"\x00"] = &copy
		}
	}
	out := make([]QuotaRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label == out[j].Label {
			if out[i].Account != out[j].Account {
				return out[i].Account < out[j].Account
			}
			return out[i].Window < out[j].Window
		}
		return out[i].Label < out[j].Label
	})
	return out
}
func quotaAccount(m fleet.Model) string {
	if m.Provider == "" || strings.EqualFold(m.Provider, "none") || strings.EqualFold(m.Provider, "unknown") {
		return ""
	}
	if m.Kind == fleet.ModelKindLocal {
		return "local"
	}
	if m.Plan != "" {
		return m.Plan
	}
	if m.APIKeyRef == "" && len(m.Usage) > 0 {
		return m.Name
	}
	return m.APIKeyRef
}
func doorTokens(now time.Time) (int64, bool) {
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			return 0, false
		}
		home = filepath.Join(h, ".bashy")
	}
	f, e := os.Open(filepath.Join(home, "broker", "audit.jsonl"))
	if e != nil {
		return 0, false
	}
	defer f.Close()
	var total int64
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var v struct {
			Time       time.Time `json:"time"`
			Prompt     int64     `json:"prompt_tokens"`
			Completion int64     `json:"completion_tokens"`
		}
		if json.Unmarshal(scanner.Bytes(), &v) == nil && !v.Time.Before(now.Add(-5*time.Hour)) && !v.Time.After(now) {
			total += v.Prompt + v.Completion
		}
	}
	return total, scanner.Err() == nil
}
