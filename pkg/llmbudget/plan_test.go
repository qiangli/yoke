package llmbudget

import (
	"reflect"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// Story 5fec08b11a5f: the fleet plan names the shared seat counter and
// supplies only the limits it KNOWS in units this meter counts; the
// environment still overrides, and an unrecorded limit stays unset (fail-open).
func TestFromFleetModelPlanUsesKnownPlanLimitsOnly(t *testing.T) {
	fm := fleet.Model{Name: "seat-model", Kind: fleet.ModelKindSubscription, Provider: "vendor", Plan: "seat"}
	p := fleet.Plan{Name: "seat", Tier: fleet.PlanTierMax, Limits: []fleet.PlanLimit{
		{Window: fleet.PlanWindowWeek, Unit: fleet.PlanUnitTokens, Value: 5000, Source: "x"},
		{Window: fleet.PlanWindow5h, Unit: fleet.PlanUnitCredits, Value: 12000, Source: "x"},
		{Window: fleet.PlanWindowNone, Unit: fleet.PlanUnitRequests, Value: 8, Source: "x"},
	}}
	m := FromFleetModelPlan(fm, p)
	if m.Plan != "seat" || m.PlanTier != fleet.PlanTierMax {
		t.Fatalf("plan/tier = %q/%q", m.Plan, m.PlanTier)
	}
	if m.Limits.WeeklyTokens != 5000 {
		t.Fatalf("weekly tokens = %d, want the plan's 5000", m.Limits.WeeklyTokens)
	}
	if m.Limits.DailyTokens != 0 || m.Limits.DailyRequests != 0 || m.Limits.WeeklyRequests != 0 {
		t.Fatalf("an unrecorded or unmetered limit was filled in: %+v", m.Limits)
	}

	t.Setenv("BASHY_LLM_MODEL_SEAT_MODEL_WEEKLY_TOKENS", "700")
	t.Setenv("BASHY_LLM_PLAN_SEAT_MODEL", "operator-seat")
	m = FromFleetModelPlan(fm, p)
	if m.Limits.WeeklyTokens != 700 || m.Plan != "operator-seat" {
		t.Fatalf("environment override lost: plan %q weekly %d", m.Plan, m.Limits.WeeklyTokens)
	}

	// No plan at all: the legacy provider counter and no limits — fail-open.
	m = FromFleetModelPlan(fleet.Model{Name: "bare", Kind: fleet.ModelKindSubscription, Provider: "vendor"}, fleet.Plan{})
	if planName(m) != "vendor:default" || m.PlanTier != "" || m.Limits.WeeklyTokens != 0 {
		t.Fatalf("unknown plan = %+v", m)
	}
	g := New(Config{Models: map[string]Model{"bare": m}, StatePath: t.TempDir() + "/m.json"})
	if d := g.Check("bare", 10); d.Action != Allow || d.Reason != "missing plan limit; fail-open" {
		t.Fatalf("unknown plan limit decision = %+v", d)
	}
}

// Heavy/manager work prefers the highest plan tier; unknowns keep their place
// at the end and are never dropped.
func TestPreferForHeavyOrdersByPlanTier(t *testing.T) {
	g := New(Config{StatePath: t.TempDir() + "/m.json", Models: map[string]Model{
		"pro-a":  {PlanTier: fleet.PlanTierPro},
		"max-a":  {PlanTier: fleet.PlanTierMax},
		"none":   {},
		"pro-b":  {PlanTier: fleet.PlanTierPro},
		"max-b":  {PlanTier: fleet.PlanTierMax},
		"entry1": {PlanTier: fleet.PlanTierEntry},
	}})
	got := g.PreferForHeavy([]string{"none", "pro-a", "max-a", "missing", "entry1", "pro-b", "max-b"})
	want := []string{"max-a", "max-b", "pro-a", "pro-b", "entry1", "none", "missing"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PreferForHeavy = %v, want %v", got, want)
	}
}
