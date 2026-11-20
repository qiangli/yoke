package llmbudget

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Regression for 682f7e66 (1b): WithCachedSourcesOnly satisfies report reads
// from existing source caches without spawning live per-account probes when a
// cache entry is missing or expired.
func TestWithCachedSourcesOnlyNeverRunsLiveProbe(t *testing.T) {
	now := time.Date(2026, 11, 20, 19, 0, 0, 0, time.UTC)
	var count atomic.Int32
	var fail atomic.Bool

	src := SourceConfig{
		ID:             "fixture-account",
		Kind:           "fixture",
		Provider:       "anthropic",
		Account:        "acct-1",
		Pool:           "acct-1",
		Lane:           LaneSubscription,
		Enabled:        true,
		RefreshSeconds: 60,
	}
	g := fixtureGate(t, &Policy{
		Version: 1,
		Sources: []SourceConfig{src},
	})
	g.cfg.Adapters = []Adapter{fixtureAdapter{count: &count, fail: &fail}}
	g.cfg.Now = func() time.Time { return now }

	findAcct := func(rep *Report) *AccountReport {
		for i := range rep.Accounts {
			if rep.Accounts[i].Account == "acct-1" {
				return &rep.Accounts[i]
			}
		}
		return nil
	}

	// Cold cache: cache-only context must return unavailable immediately with 0 probe calls.
	rep, err := g.CollectReport(WithCachedSourcesOnly(context.Background()), ReportOptions{Now: now})
	if err != nil {
		t.Fatalf("cold cache-only CollectReport: %v", err)
	}
	if got := count.Load(); got != 0 {
		t.Fatalf("cold cache-only triggered %d live adapter calls, want 0", got)
	}
	if a := findAcct(rep); a == nil || a.Status != "unavailable" {
		t.Fatalf("cold cache-only acct-1 = %+v, want unavailable", a)
	}

	// Normal (non-cache-only) call populates the source cache once.
	rep, err = g.CollectReport(context.Background(), ReportOptions{Now: now})
	if err != nil {
		t.Fatalf("live CollectReport: %v", err)
	}
	a := findAcct(rep)
	if gotCount := count.Load(); gotCount != 1 || a == nil || a.Status != "ok" {
		t.Fatalf("live CollectReport count=%d acct-1=%+v", gotCount, a)
	}

	// Expired cache: cache-only context returns stale cached observation with 0 new probe calls.
	now = now.Add(10 * time.Minute)
	rep, err = g.CollectReport(WithCachedSourcesOnly(context.Background()), ReportOptions{Now: now})
	if err != nil {
		t.Fatalf("expired cache-only CollectReport: %v", err)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("expired cache-only triggered new adapter call: count=%d, want 1", got)
	}
	if a := findAcct(rep); a == nil || a.Status != "stale" {
		t.Fatalf("expired cache-only acct-1 = %+v, want stale", a)
	}
}
