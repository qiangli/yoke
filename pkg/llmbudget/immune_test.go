package llmbudget

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestImmunePolicyDefaultsAndOverrides(t *testing.T) {
	g := New(Config{Policy: &Policy{Version: 1}})
	got, err := g.ImmuneSettingsFromPolicy()
	if err != nil || got.WarnHeadroom != .15 || got.CriticalHeadroom != .05 || got.ForecastMargin != 15*time.Minute || got.GracePeriod != 3*time.Minute {
		t.Fatalf("defaults=%+v err=%v", got, err)
	}
	g = New(Config{Policy: &Policy{Version: 1, Immune: ImmunePolicy{WarnHeadroom: .3, CriticalHeadroom: .1, ForecastMargin: "20m", GracePeriod: "45s"}}})
	got, err = g.ImmuneSettingsFromPolicy()
	if err != nil || got.WarnHeadroom != .3 || got.CriticalHeadroom != .1 || got.ForecastMargin != 20*time.Minute || got.GracePeriod != 45*time.Second {
		t.Fatalf("override=%+v err=%v", got, err)
	}
}

func TestQuotaRowsExposeActiveImmuneLevel(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	used := 90.0
	report := &Report{Accounts: []AccountReport{{Provider: "vendor", Account: "seat", Pool: "seat", Lane: LaneSubscription, AccountKnown: true, Models: []string{"model"}, Metrics: []Metric{{Name: "quota.used_percent", Value: &used, Window: "week", Classification: "measured", Source: "fixture", ObservedAt: now}}}}}
	models := []fleet.Model{{Name: "model", Provider: "vendor", Plan: "seat", Kind: fleet.ModelKindSubscription}}
	plans := []fleet.Plan{{Name: "seat", Vendor: "vendor"}}
	agents := []fleet.Agent{{Name: "agent", Tool: "tool", Model: "model"}}
	policy := &Policy{Version: 1}
	rows := quotaRows(report, models, plans, agents, policy, QuotaOptions{}, now)
	if len(rows) != 1 || rows[0].Immune != "warn" {
		t.Fatalf("warn rows=%+v", rows)
	}
	used = 96
	rows = quotaRows(report, models, plans, agents, policy, QuotaOptions{}, now)
	if len(rows) != 1 || rows[0].Immune != "critical" {
		t.Fatalf("critical rows=%+v", rows)
	}
}

func TestImmunePolicyRejectsInvalidThresholds(t *testing.T) {
	for _, immune := range []ImmunePolicy{
		{WarnHeadroom: .1, CriticalHeadroom: .2},
		{WarnHeadroom: 1.1},
		{ForecastMargin: "soon"},
		{GracePeriod: "-1m"},
	} {
		g := New(Config{Policy: &Policy{Version: 1, Immune: immune}})
		if _, err := g.ImmuneSettingsFromPolicy(); err == nil {
			t.Fatalf("accepted invalid immune policy: %+v", immune)
		}
	}
}
