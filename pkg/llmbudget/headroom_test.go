package llmbudget

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

type countedHeadroomAdapter struct {
	calls *int
	value float64
}

func (countedHeadroomAdapter) Kind() string { return "counted-headroom" }
func (a countedHeadroomAdapter) Collect(_ context.Context, _ SourceConfig, now time.Time) (SourceResult, error) {
	*a.calls = *a.calls + 1
	return SourceResult{Status: "ok", Metrics: []Metric{{Name: "quota.used_percent", Value: &a.value, Classification: "measured", Source: "fixture", ObservedAt: now}}}, nil
}

func TestCachedMeasuredHeadroomReadsCacheWithoutRefreshing(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	calls := 0
	c := SourceConfig{ID: "cached-seat", Kind: "counted-headroom", Provider: "vendor", Account: "seat", Pool: "seat", Lane: LaneSubscription, Enabled: true, RefreshSeconds: 300}
	g := New(Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Now: func() time.Time { return now }, Policy: &Policy{Version: 1, Bindings: []Binding{{Model: "cached-model", Provider: "vendor", Account: "seat", Pool: "seat", Lane: LaneSubscription}}, Sources: []SourceConfig{c}}, Adapters: []Adapter{countedHeadroomAdapter{calls: &calls, value: 97}}})
	if _, _, known, e := g.CachedMeasuredHeadroom(context.Background(), "cached-model", ""); e != nil || known || calls != 0 {
		t.Fatalf("cold cache called adapter: known=%v calls=%d err=%v", known, calls, e)
	}
	if _, e := g.CollectReport(context.Background(), ReportOptions{Roster: []Binding{{Model: "cached-model", Provider: "vendor", Account: "seat", Pool: "seat", Lane: LaneSubscription, AccountKnown: true}}, Now: now}); e != nil || calls != 1 {
		t.Fatalf("foreground prime: calls=%d err=%v", calls, e)
	}
	h, _, known, e := g.CachedMeasuredHeadroom(context.Background(), "cached-model", "")
	if e != nil || !known || math.Abs(h-.03) > 1e-9 || calls != 1 {
		t.Fatalf("cache headroom %.9f %v calls=%d err=%v", h, known, calls, e)
	}
	now = now.Add(6 * time.Minute)
	if _, _, known, e := g.CachedMeasuredHeadroom(context.Background(), "cached-model", ""); e != nil || known || calls != 1 {
		t.Fatalf("expired cache refreshed: known=%v calls=%d err=%v", known, calls, e)
	}
	if h, _, known, e := g.MeasuredHeadroom(context.Background(), "cached-model", ""); e != nil || !known || math.Abs(h-.03) > 1e-9 || calls != 2 {
		t.Fatalf("measured refresh %.9f %v calls=%d err=%v", h, known, calls, e)
	}
}

type headroomAdapter struct {
	metrics []Metric
}

func (headroomAdapter) Kind() string { return "headroom-fixture" }
func (a headroomAdapter) Collect(_ context.Context, _ SourceConfig, _ time.Time) (SourceResult, error) {
	return SourceResult{Status: "ok", Metrics: a.metrics}, nil
}

func TestSubscriptionHeadroomUsesMeasuredAccountWindow(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	usedFiveHour, usedWeek := 35.0, 80.0
	policy := &Policy{Version: 1,
		Bindings: []Binding{{Model: "headroom-fixture-model", Agent: "claude-seat", Provider: "anthropic", Account: "acct", Pool: "pro", Lane: LaneSubscription}},
		Sources:  []SourceConfig{{ID: "claude", Kind: "headroom-fixture", Provider: "anthropic", Account: "acct", Pool: "pro", Lane: LaneSubscription, Enabled: true}},
	}
	g := New(Config{Policy: policy, Now: func() time.Time { return now }, Adapters: []Adapter{headroomAdapter{metrics: []Metric{
		{Name: "quota.used_percent", Value: &usedFiveHour, Unit: "percent", Classification: "actual", Source: "fixture:five_hour", ObservedAt: now},
		{Name: "quota.used_percent", Value: &usedWeek, Unit: "percent", Classification: "actual", Source: "fixture:seven_day", ObservedAt: now},
	}}}})

	got, known, err := g.SubscriptionHeadroom(context.Background(), Binding{Model: "headroom-fixture-model", Agent: "claude-seat", Provider: "anthropic"})
	if err != nil || !known || math.Abs(got-.20) > 1e-9 {
		t.Fatalf("SubscriptionHeadroom() = %v, %v, %v; want .20, true, nil", got, known, err)
	}
}

func TestMeasuredHeadroomResolvesOnlyAnObservedSubscriptionBinding(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	used := 40.0
	g := New(Config{StatePath: filepath.Join(t.TempDir(), "meter.json"), Now: func() time.Time { return now }, Adapters: []Adapter{headroomAdapter{metrics: []Metric{{Name: "quota.used_percent", Value: &used, Classification: "actual", Source: "fixture", ObservedAt: now}}}}, Policy: &Policy{Version: 1,
		Bindings: []Binding{{Model: "model-b", Agent: "vendor-b:model-b", Provider: "vendor-b", Account: "seat", Pool: "primary", Lane: LaneSubscription}},
		Sources:  []SourceConfig{{ID: "seat", Kind: "headroom-fixture", Provider: "vendor-b", Account: "seat", Pool: "primary", Lane: LaneSubscription, Enabled: true}},
	}})
	if _, e := g.CollectReport(context.Background(), ReportOptions{Roster: []Binding{{Model: "model-b", Provider: "vendor-b", Account: "seat", Pool: "primary", Lane: LaneSubscription, AccountKnown: true}}, Now: now}); e != nil {
		t.Fatal(e)
	}
	remaining, binding, known, err := g.MeasuredHeadroom(context.Background(), "model-b", "vendor-b:model-b")
	if err != nil || !known || remaining != .6 || binding.Account != "seat" {
		t.Fatalf("MeasuredHeadroom() = %v, %+v, %v, %v", remaining, binding, known, err)
	}
	if _, _, known, err = g.MeasuredHeadroom(context.Background(), "missing", "vendor-b:missing"); err != nil || known {
		t.Fatalf("missing binding = known=%v err=%v; want false, nil", known, err)
	}
}

func TestSubscriptionHeadroomRejectsUnknownAndStaleMetrics(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	used := 25.0
	for _, classification := range []string{"unknown", "stale", "estimated"} {
		t.Run(classification, func(t *testing.T) {
			policy := &Policy{Version: 1,
				Bindings: []Binding{{Model: "headroom-fixture-model", Agent: "claude-seat", Provider: "anthropic", Account: "acct", Pool: "pro", Lane: LaneSubscription}},
				Sources:  []SourceConfig{{ID: "claude", Kind: "headroom-fixture", Provider: "anthropic", Account: "acct", Pool: "pro", Lane: LaneSubscription, Enabled: true}},
			}
			g := New(Config{Policy: policy, Now: func() time.Time { return now }, Adapters: []Adapter{headroomAdapter{metrics: []Metric{{
				Name: "quota.used_percent", Value: &used, Unit: "percent", Classification: classification, Source: "fixture", ObservedAt: now,
			}}}}})
			got, known, err := g.SubscriptionHeadroom(context.Background(), Binding{Model: "headroom-fixture-model", Agent: "claude-seat", Provider: "anthropic"})
			if err != nil || known || got != 0 {
				t.Fatalf("SubscriptionHeadroom() = %v, %v, %v; want 0, false, nil", got, known, err)
			}
		})
	}
}
