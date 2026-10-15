package llmbudget

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/quotaobs"
)

func TestQuotaObservedPrecedence(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	state := ObservedState{Account: "seat", Kind: "exhausted", Window: "5h", ResetAt: &reset, Classification: "observed", Signal: "vendor-429", At: now.Add(-time.Minute)}
	for _, tc := range []struct {
		name, classification, status string
		stale                        bool
	}{
		{"measured wins", "measured", "ok", false},
		{"stale measured yields", "measured", "exhausted", true},
		{"inferred yields", "inferred", "exhausted", false},
		{"predicted yields", "predicted", "exhausted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []QuotaRow{{Account: "seat", Window: "5h", Status: "ok", Classification: tc.classification, Stale: tc.stale}}
			applyObserved(rows, []ObservedState{state}, now)
			if rows[0].Status != tc.status {
				t.Fatalf("row: %+v", rows[0])
			}
			if tc.status == "exhausted" && (rows[0].Classification != "observed" || !strings.Contains(rows[0].Source, "vendor-429")) {
				t.Fatalf("observation missing: %+v", rows[0])
			}
		})
	}
}

func TestQuotaLogFilter(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(time.Hour)
	for _, o := range []quotaobs.Observation{
		{At: now.Add(-2 * time.Hour), Source: "weave", Account: "a", Excerpt: "first"},
		{At: now.Add(-time.Hour), Source: "door", Account: "b", Excerpt: "other"},
		{At: now, Source: "cligw", Account: "a", Excerpt: "last", Parsed: &quotaobs.ParsedSignal{Kind: "exhausted", ResetAt: &reset}, Analysis: &quotaobs.Analysis{Kind: "exhausted", ResetAt: &reset, Summary: "limit reached"}},
	} {
		if err := quotaobs.Record(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	got := QuotaLog(ctx, LogFilter{Account: "a", Since: now.Add(-3 * time.Hour), Limit: 1})
	if len(got) != 1 || got[0].Excerpt != "last" || got[0].Classification != "observed" || got[0].Analysis != "limit reached" {
		t.Fatalf("log: %+v", got)
	}
	got = QuotaLog(ctx, LogFilter{Since: now.Add(-90 * time.Minute)})
	if len(got) != 2 || got[0].Account != "a" || got[1].Account != "b" {
		t.Fatalf("since: %+v", got)
	}
}

func TestObservedExhaustionSeatRule(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, class, kind string
		reset             time.Time
		want              bool
	}{
		{"confirmed", "observed", "exhausted", now.Add(time.Hour), true},
		{"unconfirmed", "observed-unconfirmed", "exhausted", now.Add(time.Hour), false},
		{"inferred", "inferred", "exhausted", now.Add(time.Hour), false},
		{"conflict", "conflict", "exhausted", now.Add(time.Hour), false},
		{"predicted", "predicted", "exhausted", now.Add(time.Hour), false},
		{"warning", "observed", "warning", now.Add(time.Hour), false},
		{"expired", "observed", "exhausted", now.Add(-time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, got := observedExhaustion("seat", []ObservedState{{Account: "seat", Classification: tc.class, Kind: tc.kind, ResetAt: &tc.reset}}, now)
			if got != tc.want {
				t.Fatalf("got %t, want %t", got, tc.want)
			}
		})
	}
}
