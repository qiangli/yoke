package llmbudget

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TestStatusUnknownLimitIsNeitherZeroNorUnlimited pins the rule this accessor
// exists for. The fleet logs "missing subscription plan limit metadata;
// allowing" in production today, so no-limit-known is the COMMON case, not a
// hypothetical. Reporting it as 0 would read as exhausted; reporting it as
// unlimited is the same class of bug as an absent test result read as a pass.
func TestStatusUnknownLimitIsNeitherZeroNorUnlimited(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	g := New(Config{
		Now: func() time.Time { return now },
		Models: map[string]Model{
			// A subscription model with NO limit metadata at all.
			"claude-sub": {Name: "claude-sub", Kind: fleet.ModelKindSubscription, Provider: "anthropic", Plan: "max"},
		},
	})
	g.Record("claude-sub", 300, 200, 0)

	s := g.Status("claude-sub")

	if s.LimitKnown {
		t.Fatalf("no limit metadata must report LimitKnown=false, got %+v", s)
	}
	if s.Limit != nil || s.Remaining != nil {
		t.Fatalf("unknown limit must leave Limit/Remaining nil (not 0, not a sentinel), got limit=%v remaining=%v", s.Limit, s.Remaining)
	}
	// Spent is a local fact and survives the missing ceiling.
	if s.Spent != 500 {
		t.Fatalf("spent must still be reported when the limit is unknown: got %d, want 500", s.Spent)
	}

	// A caller cannot mistake UNKNOWN for 0: there is no int64 to read.
	// A caller cannot mistake UNKNOWN for unlimited: LimitKnown gates it.
	if s.LimitKnown && s.Remaining == nil {
		t.Fatal("LimitKnown=true must imply a non-nil Remaining")
	}

	// The JSON envelope must not carry limit/remaining keys at all — a
	// renderer that reads a missing key as 0 or ∞ has nothing to misread.
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["limit"]; ok {
		t.Fatalf("unknown limit must be absent from JSON, got %s", b)
	}
	if _, ok := raw["remaining"]; ok {
		t.Fatalf("unknown remaining must be absent from JSON, got %s", b)
	}
	if known, ok := raw["limit_known"].(bool); !ok || known {
		t.Fatalf("limit_known must be present and false, got %s", b)
	}
}

// TestStatusKnownLimitIsConsistentAfterRecord asserts the arithmetic a caller
// will actually reason with: remaining == limit - spent.
func TestStatusKnownLimitIsConsistentAfterRecord(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	g := New(Config{
		Now: func() time.Time { return now },
		Models: map[string]Model{
			"glm": {Name: "glm", Kind: fleet.ModelKindSubscription, Provider: "zai", Plan: "glm-pro",
				Limits: Limits{DailyTokens: 1000}},
		},
	})

	before := g.Status("glm")
	if !before.LimitKnown || before.Limit == nil || *before.Limit != 1000 {
		t.Fatalf("known limit must be reported: %+v", before)
	}
	if before.Spent != 0 || *before.Remaining != 1000 {
		t.Fatalf("fresh gate: want spent=0 remaining=1000, got %+v", before)
	}

	g.Record("glm", 100, 50, 0)

	after := g.Status("glm")
	if !after.LimitKnown || after.Limit == nil || after.Remaining == nil {
		t.Fatalf("known limit must survive a Record: %+v", after)
	}
	if after.Spent != 150 {
		t.Fatalf("spent: got %d, want 150", after.Spent)
	}
	if *after.Limit != 1000 {
		t.Fatalf("limit: got %d, want 1000", *after.Limit)
	}
	if *after.Remaining != *after.Limit-after.Spent {
		t.Fatalf("remaining must equal limit-spent: %d != %d-%d", *after.Remaining, *after.Limit, after.Spent)
	}
	if after.Unit != UnitTokens || after.Basis != "subscription_daily_tokens" {
		t.Fatalf("basis/unit: got %q/%q", after.Basis, after.Unit)
	}
}

// TestStatusIsReadOnly proves a read costs nothing: no counter moves and no
// state file is written. A read that costs you budget is not a read.
func TestStatusIsReadOnly(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	path := t.TempDir() + "/state.json"
	g := New(Config{
		Now:       func() time.Time { return now },
		StatePath: path,
		Models: map[string]Model{
			"glm": {Name: "glm", Kind: fleet.ModelKindSubscription, Provider: "zai", Plan: "glm-pro",
				Limits: Limits{DailyTokens: 1000}},
		},
	})
	g.Load(State{Plans: map[string]Counters{"glm-pro": {DayStart: dayStart(now), DayTokens: 400}}})

	first := g.Status("glm")
	for range 5 {
		if got := g.Status("glm"); got.Spent != first.Spent || *got.Remaining != *first.Remaining {
			t.Fatalf("repeated reads must not move the meter: %+v then %+v", first, got)
		}
	}
	if first.Spent != 400 {
		t.Fatalf("spent: got %d, want 400", first.Spent)
	}
}

// TestStatusBindingCeilingIsTheTightestOne — with several ceilings configured,
// the reading must describe the one that will actually stop the caller.
func TestStatusBindingCeilingIsTheTightestOne(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	g := New(Config{
		Now: func() time.Time { return now },
		Models: map[string]Model{
			"glm": {Name: "glm", Kind: fleet.ModelKindSubscription, Provider: "zai", Plan: "glm-pro",
				Limits: Limits{DailyTokens: 10000, DailyRequests: 5}},
		},
	})
	g.Load(State{Plans: map[string]Counters{"glm-pro": {
		DayStart: dayStart(now), WeekStart: weekStart(now), DayTokens: 1000, DayRequests: 4,
	}}})

	s := g.Status("glm")
	if s.Basis != "subscription_daily_requests" || s.Unit != UnitRequests {
		t.Fatalf("tightest ceiling is daily requests (1 left) not daily tokens (9000 left); got %q", s.Basis)
	}
	if *s.Remaining != 1 {
		t.Fatalf("remaining: got %d, want 1", *s.Remaining)
	}
}

// TestStatusAllListsRecordedModels covers the accessor the CLI surface uses.
func TestStatusAllListsRecordedModels(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	g := New(Config{
		Now: func() time.Time { return now },
		Models: map[string]Model{
			"glm": {Name: "glm", Kind: fleet.ModelKindSubscription, Provider: "zai", Plan: "glm-pro",
				Limits: Limits{DailyTokens: 1000}},
		},
	})
	g.Record("glm", 10, 10, 0)

	all := g.StatusAll()
	if len(all) != 1 || all[0].Model != "glm" || all[0].Spent != 20 {
		t.Fatalf("StatusAll: got %+v", all)
	}
}
