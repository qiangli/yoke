package llmbudget

import (
	"context"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestCheckDecisions(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		model      Model
		state      State
		est        int64
		override   bool
		wantAction Action
		wantModel  string
		wantBounds int
	}{
		{
			name: "api over budget downgrades",
			model: Model{Name: "premium", Kind: fleet.ModelKindAPI, Billing: fleet.BillingMetered, Provider: "openai",
				CostMicro: 1000, DowngradeTo: "cheap", Limits: Limits{BudgetUSD: 1}},
			est: 2000, wantAction: Downgrade, wantModel: "cheap", wantBounds: 1,
		},
		{
			name: "api over budget blocks without cheaper leg",
			model: Model{Name: "premium", Kind: fleet.ModelKindAPI, Billing: fleet.BillingMetered, Provider: "openai",
				CostMicro: 1000, Limits: Limits{BudgetUSD: 1}},
			est: 2000, wantAction: Block, wantModel: "premium", wantBounds: 1,
		},
		{
			name: "api provider quota blocks",
			model: Model{Name: "api", Kind: fleet.ModelKindAPI, Billing: fleet.BillingMetered, Provider: "deepseek",
				CostMicro: 1000, Limits: Limits{ProviderQuotaUSD: 1}},
			state: State{Providers: map[string]Counters{"deepseek": {DayStart: dayStart(now), DayCostUSD: 0.75, CostUSD: 0.75}}},
			est:   300, wantAction: Block, wantModel: "api", wantBounds: 1,
		},
		{
			name: "api rate limit queues",
			model: Model{Name: "api", Kind: fleet.ModelKindAPI, Billing: fleet.BillingMetered, Provider: "deepseek",
				CostMicro: 1, Limits: Limits{RateTokens: 100, RatePer: time.Minute}},
			state: State{Buckets: map[string]Bucket{"deepseek": {WindowStart: now, Used: 90}}},
			est:   20, wantAction: Queue, wantModel: "api", wantBounds: 1,
		},
		{
			name: "subscription rate limit queues",
			model: Model{Name: "claude", Kind: fleet.ModelKindSubscription, Billing: fleet.BillingFlatThenMetered, Provider: "anthropic",
				Plan: "max", Limits: Limits{RateTokens: 100, RatePer: time.Minute}},
			state: State{Buckets: map[string]Bucket{"anthropic": {WindowStart: now, Used: 95}}},
			est:   10, wantAction: Queue, wantModel: "claude", wantBounds: 1,
		},
		{
			name: "subscription near daily ceiling routes alternative",
			model: Model{Name: "glm", Kind: fleet.ModelKindAPI, Billing: fleet.BillingFlat, Provider: "zai", Plan: "glm-pro",
				RouteAltTo: "claude-sub", Limits: Limits{DailyTokens: 1000, NearLimitRatio: 0.9}},
			state: State{Plans: map[string]Counters{"glm-pro": {DayStart: dayStart(now), DayTokens: 850}}},
			est:   50, wantAction: RouteAlt, wantModel: "claude-sub", wantBounds: 1,
		},
		{
			name: "subscription weekly ceiling routes alternative",
			model: Model{Name: "codex", Kind: fleet.ModelKindSubscription, Billing: fleet.BillingFlatThenMetered, Provider: "openai", Plan: "codex-pro",
				RouteAltTo: "glm", Limits: Limits{WeeklyRequests: 10, NearLimitRatio: 0.9}},
			state: State{Plans: map[string]Counters{"codex-pro": {WeekStart: weekStart(now), WeekRequests: 8}}},
			est:   1, wantAction: RouteAlt, wantModel: "glm", wantBounds: 1,
		},
		{
			name: "override bypasses binds",
			model: Model{Name: "premium", Kind: fleet.ModelKindAPI, Billing: fleet.BillingMetered, Provider: "openai",
				CostMicro: 1000, Limits: Limits{BudgetUSD: 1, RateTokens: 1, RatePer: time.Minute}},
			est: 2000, override: true, wantAction: Allow, wantModel: "premium",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bounds := 0
			g := New(Config{
				Models:       map[string]Model{tt.model.Name: tt.model},
				Now:          func() time.Time { return now },
				AllowPremium: tt.override,
				BoundHit: func(context.Context, string, int64, int64, string) {
					bounds++
				},
			})
			g.state = tt.state
			g.loaded = true
			if g.state.Models == nil {
				g.state.Models = map[string]Counters{}
			}
			if g.state.Plans == nil {
				g.state.Plans = map[string]Counters{}
			}
			if g.state.Providers == nil {
				g.state.Providers = map[string]Counters{}
			}
			if g.state.Buckets == nil {
				g.state.Buckets = map[string]Bucket{}
			}
			got := g.CheckContext(context.Background(), tt.model.Name, tt.est)
			if got.Action != tt.wantAction || got.Model != tt.wantModel {
				t.Fatalf("decision = %s/%s (%s), want %s/%s", got.Action, got.Model, got.Reason, tt.wantAction, tt.wantModel)
			}
			if bounds != tt.wantBounds {
				t.Fatalf("BoundHit count = %d, want %d", bounds, tt.wantBounds)
			}
		})
	}
}

func TestRecordAccumulatesCostAndPlanWindows(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	g := New(Config{
		Models: map[string]Model{
			"glm": {Name: "glm", Kind: fleet.ModelKindAPI, Billing: fleet.BillingFlat, Provider: "zai", Plan: "glm-pro"},
		},
		Now:      func() time.Time { return now },
		BoundHit: func(context.Context, string, int64, int64, string) {},
	})
	g.Record("glm", 100, 25, 0.42)
	g.Record("glm", 50, 25, 0.08)
	m := g.state.Models["glm"]
	if m.DayTokens != 200 || m.WeekTokens != 200 || m.DayRequests != 2 || m.WeekRequests != 2 ||
		m.DayCostUSD != 0.50 || m.WeekCostUSD != 0.50 || m.CostUSD != 0.50 {
		t.Fatalf("model counters = %+v", m)
	}
	p := g.state.Plans["glm-pro"]
	if p.DayTokens != 200 || p.WeekTokens != 200 || p.DayRequests != 2 || p.WeekRequests != 2 || p.CostUSD != 0 {
		t.Fatalf("plan counters = %+v", p)
	}

	g.cfg.Now = func() time.Time { return now.AddDate(0, 0, 1) }
	g.Record("glm", 10, 0, 0.01)
	m = g.state.Models["glm"]
	if m.DayTokens != 10 || m.WeekTokens != 210 || m.DayRequests != 1 || m.WeekRequests != 3 ||
		m.DayCostUSD != 0.01 || m.WeekCostUSD != 0.51 || m.CostUSD != 0.51 {
		t.Fatalf("rolled counters = %+v", m)
	}
}
