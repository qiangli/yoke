package llmbudget

import (
	"context"
	"math"
	"testing"
	"time"
)

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
