package llmbudget

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

type QuotaRow struct {
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
	Status           string     `json:"status"`
	Classification   string     `json:"classification,omitempty"`
	Headroom         *float64   `json:"headroom,omitempty"`
	SuggestedWorkers *int       `json:"suggested_workers,omitempty"`
}
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
	report, err := g.CollectReport(ctx, ReportOptions{Provider: opt.Provider, Refresh: opt.Refresh, Now: now})
	if err != nil {
		return nil, err
	}
	cat := fleet.New()
	models, _ := cat.Models()
	plans, _ := cat.Plans()
	byPlan := map[string]fleet.Plan{}
	for _, p := range plans {
		byPlan[p.Name] = p
	}
	p, err := g.policy()
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	configured := map[string]bool{}
	modelByProvider := map[string]fleet.Model{}
	for _, m := range models {
		provider := strings.ToLower(m.Provider)
		if provider == "" {
			continue
		}
		configured[provider] = true
		if m.Active() {
			active[provider] = true
			if _, ok := modelByProvider[provider]; !ok {
				modelByProvider[provider] = m
			}
		}
	}
	rows := map[string]*QuotaRow{}
	tokenParts := map[string]float64{}
	tokenTotals := map[string]float64{}
	hasParts := map[string]bool{}
	for _, account := range report.Accounts {
		provider := strings.ToLower(account.Provider)
		if opt.Provider != "" && !strings.EqualFold(provider, opt.Provider) {
			continue
		}
		for _, metric := range account.Metrics {
			if metric.Value == nil || metric.Source == "unavailable" {
				continue
			}
			if metric.Name != "quota.used_percent" && metric.Name != "usage.tokens" && metric.Name != "usage.tokens_in" && metric.Name != "usage.tokens_out" && metric.Name != "usage.balance" && metric.Name != "usage.used" && metric.Name != "usage.limit" && metric.Name != "usage.remaining" {
				continue
			}
			window := metric.Window
			key := provider + "\x00" + window
			row := rows[key]
			if row == nil {
				row = &QuotaRow{Provider: provider, Window: window, Lane: account.Lane, Status: "ok"}
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
			case "usage.tokens":
				tokenTotals[key] += *metric.Value
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
		} else if v, ok := tokenTotals[key]; ok {
			row.Tokens = &v
		}
	}
	for _, r := range rows {
		if m, ok := modelByProvider[r.Provider]; ok {
			r.Plan = m.Plan
		}
		if r.Headroom != nil {
			limit := 0
			limitKnown := false
			if m, ok := modelByProvider[r.Provider]; ok {
				if n, found := byPlan[m.Plan].Limit("concurrent", "requests", m.Name); found {
					limit = int(n)
					limitKnown = true
				}
			}
			if !limitKnown {
				for _, c := range p.Constraints {
					if (c.Provider == r.Provider || c.Provider == "") && c.Host == "" && c.Concurrency != nil {
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
	for provider := range configured {
		if opt.Provider != "" && !strings.EqualFold(provider, opt.Provider) {
			continue
		}
		if !active[provider] && opt.All {
			for key, row := range rows {
				if row.Provider == provider {
					delete(rows, key)
				}
			}
			rows[provider+"\x00"] = &QuotaRow{Provider: provider, Status: "not-configured"}
			continue
		}
		found := false
		for _, r := range rows {
			if r.Provider == provider {
				found = true
				break
			}
		}
		if !found && (active[provider] || opt.All) {
			status := "unmeasured"
			if !active[provider] {
				status = "not-configured"
			}
			rows[provider+"\x00"] = &QuotaRow{Provider: provider, Status: status}
		}
	}
	if opt.Provider == "" || strings.EqualFold(opt.Provider, "local") {
		if tokens, ok := doorTokens(now); ok {
			v := float64(tokens)
			row := rows["local\x005h"]
			if row == nil {
				row = &QuotaRow{Provider: "local", Lane: LaneLocal, Window: "5h", Status: "ok"}
				rows["local\x005h"] = row
			}
			row.Tokens = &v
			row.Source = "bashy-door"
			row.Classification = "measured"
			row.ObservedAt = &now
		}
	}
	out := make([]QuotaRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].Window < out[j].Window
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
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
	return total, true
}
