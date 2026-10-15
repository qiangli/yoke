package fleet

import (
	"strings"
	"testing"
	"time"
)

// fixedNow anchors every clock-shaped reset: 2026-06-24 10:00:00 UTC (a
// Wednesday). Local-time parsers resolve against now.Location(), so UTC keeps
// the expectations deterministic on every host.
var signalsNow = time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

// seedPlans loads the embedded baseline plans by name.
func seedPlans(t *testing.T) map[string]Plan {
	t.Helper()
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
	plans, errs := c.Plans()
	if len(errs) != 0 {
		t.Fatalf("seed plans parse: %v", errs)
	}
	out := map[string]Plan{}
	for _, p := range plans {
		out[p.Name] = p
	}
	return out
}

func seedTools(t *testing.T) map[string]Tool {
	t.Helper()
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
	tools, errs := c.Tools(true)
	if len(errs) != 0 {
		t.Fatalf("seed tools parse: %v", errs)
	}
	out := map[string]Tool{}
	for _, tl := range tools {
		out[tl.Name] = tl
	}
	return out
}

func findSignal(t *testing.T, got []ParsedSignal, id string) ParsedSignal {
	t.Helper()
	for _, ps := range got {
		if ps.Signal == id {
			return ps
		}
	}
	t.Fatalf("signal %q did not match; got %+v", id, got)
	return ParsedSignal{}
}

func assertNoSignal(t *testing.T, got []ParsedSignal, id string) {
	t.Helper()
	for _, ps := range got {
		if ps.Signal == id {
			t.Fatalf("signal %q matched but should not have; got %+v", id, got)
		}
	}
}

// One realistic (synthetic, redacted) observation per seeded matcher, run
// against the baseline YAML the seeds actually ship — not a Go copy of them.
func TestUsageSignalSeedMatch(t *testing.T) {
	plans := seedPlans(t)
	tools := seedTools(t)

	cases := []struct {
		name    string
		signals []UsageSignal
		excerpt string
		headers map[string]string
		want    []ParsedSignal // Signal/Kind/Window filled; pointers compared by value
		drop    []string       // ids that must NOT match
	}{
		{
			name:    "codex 5h bare clock",
			signals: tools["codex"].UsageSignals,
			excerpt: `ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 11:41 AM.`,
			want: []ParsedSignal{{
				Signal:  "usage-limit-5h",
				Kind:    UsageSignalKindExhausted,
				Window:  PlanWindow5h,
				ResetAt: ptrTime(time.Date(2026, 6, 24, 11, 41, 0, 0, time.UTC)),
			}},
			drop: []string{"usage-limit-week"},
		},
		{
			name:    "codex weekly dated reset",
			signals: tools["codex"].UsageSignals,
			excerpt: `ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Jul 24th, 2026 9:45 PM.`,
			want: []ParsedSignal{{
				Signal:  "usage-limit-week",
				Kind:    UsageSignalKindExhausted,
				Window:  PlanWindowWeek,
				ResetAt: ptrTime(time.Date(2026, 7, 24, 21, 45, 0, 0, time.UTC)),
			}},
			drop: []string{"usage-limit-5h"},
		},
		{
			name:    "zai 1302 rate limit",
			signals: plans["zai-glm-coding-pro"].UsageSignals,
			excerpt: `{"error":{"code":"1302","message":"Rate limit reached for requests"}}`,
			want: []ParsedSignal{{
				Signal: "1302",
				Kind:   UsageSignalKindRateLimited,
			}},
		},
		{
			// The real body shape measured on this host 2026-10-12, with a
			// synthetic timestamp.
			name:    "zai 1308 usage limit with reset",
			signals: plans["zai-glm-coding-pro"].UsageSignals,
			excerpt: `{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2026-10-12 17:34:44"}}`,
			want: []ParsedSignal{{
				Signal:  "1308",
				Kind:    UsageSignalKindExhausted,
				Window:  PlanWindow5h,
				ResetAt: ptrTime(time.Date(2026, 10, 12, 17, 34, 44, 0, time.UTC)),
			}},
		},
		{
			name:    "google resource exhausted with retryDelay",
			signals: plans["google-ai-pro"].UsageSignals,
			excerpt: `{"error":{"code":429,"message":"Quota exceeded for quota metric a generate_content quota and limit","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"37s"}]}}`,
			want: []ParsedSignal{
				{
					Signal:     "resource-exhausted",
					Kind:       UsageSignalKindExhausted,
					RetryAfter: ptrDuration(37 * time.Second),
					ResetAt:    ptrTime(signalsNow.Add(37 * time.Second)),
				},
				{
					Signal: "quota-metric",
					Kind:   UsageSignalKindExhausted,
				},
			},
		},
		{
			name:    "claude usage limit with clock reset",
			signals: plans["claude-max"].UsageSignals,
			excerpt: `Claude usage limit reached. Your limit will reset at 11:30 PM`,
			want: []ParsedSignal{{
				Signal:  "usage-limit-reset",
				Kind:    UsageSignalKindExhausted,
				Window:  PlanWindow5h,
				ResetAt: ptrTime(time.Date(2026, 6, 24, 23, 30, 0, 0, time.UTC)),
			}},
		},
		{
			name:    "claude 5-hour limit reached",
			signals: plans["claude-max"].UsageSignals,
			excerpt: `5-hour limit reached. Your session will pause until the window resets.`,
			want: []ParsedSignal{{
				Signal: "five-hour-limit",
				Kind:   UsageSignalKindExhausted,
				Window: PlanWindow5h,
			}},
		},
		{
			name:    "claude weekly percent warning",
			signals: plans["claude-max"].UsageSignals,
			excerpt: `You've used 78% of your weekly limit`,
			want: []ParsedSignal{{
				Signal:      "weekly-percent",
				Kind:        UsageSignalKindWarning,
				Window:      PlanWindowWeek,
				UsedPercent: ptrFloat(78),
			}},
		},
		{
			// Mixed-case header name: the lookup is case-insensitive both ways.
			name:    "claude unified reset header (epoch)",
			signals: plans["claude-max"].UsageSignals,
			headers: map[string]string{"Anthropic-RateLimit-Unified-Reset": "1791609000"},
			want: []ParsedSignal{{
				Signal:  "unified-reset",
				Kind:    UsageSignalKindExhausted,
				ResetAt: ptrTime(time.Unix(1791609000, 0)),
			}},
		},
		{
			name:    "openai reset headers (relative durations)",
			signals: plans["chatgpt-pro"].UsageSignals,
			headers: map[string]string{
				"x-ratelimit-reset-requests": "6m0s",
				"X-RateLimit-Reset-Tokens":   "1h0m0s",
			},
			want: []ParsedSignal{
				{
					Signal:     "reset-requests",
					Kind:       UsageSignalKindRateLimited,
					RetryAfter: ptrDuration(6 * time.Minute),
					ResetAt:    ptrTime(signalsNow.Add(6 * time.Minute)),
				},
				{
					Signal:     "reset-tokens",
					Kind:       UsageSignalKindRateLimited,
					RetryAfter: ptrDuration(time.Hour),
					ResetAt:    ptrTime(signalsNow.Add(time.Hour)),
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchSignals(tc.signals, tc.excerpt, tc.headers, signalsNow)
			if len(got) != len(tc.want) {
				t.Fatalf("matched %d signals, want %d: %+v", len(got), len(tc.want), got)
			}
			for _, want := range tc.want {
				ps := findSignal(t, got, want.Signal)
				if ps.Kind != want.Kind {
					t.Errorf("%s: kind=%q want %q", want.Signal, ps.Kind, want.Kind)
				}
				if ps.Window != want.Window {
					t.Errorf("%s: window=%q want %q", want.Signal, ps.Window, want.Window)
				}
				if !equalFloat(ps.UsedPercent, want.UsedPercent) {
					t.Errorf("%s: used=%v want %v", want.Signal, ps.UsedPercent, want.UsedPercent)
				}
				if !equalTimePtr(ps.ResetAt, want.ResetAt) {
					t.Errorf("%s: reset=%v want %v", want.Signal, ps.ResetAt, want.ResetAt)
				}
				if !equalDurationPtr(ps.RetryAfter, want.RetryAfter) {
					t.Errorf("%s: retry_after=%v want %v", want.Signal, ps.RetryAfter, want.RetryAfter)
				}
			}
			for _, id := range tc.drop {
				assertNoSignal(t, got, id)
			}
		})
	}
}

// meta-muse-pro deliberately ships NO usage signals: a sweep of this host's
// weave logs found no muse limit wording to seed (recorded in the story
// commit). Pin that so the absence is a decision, not an omission.
func TestMusePlanHasNoUsageSignals(t *testing.T) {
	p, ok := seedPlans(t)["meta-muse-pro"]
	if !ok {
		t.Fatal("meta-muse-pro plan missing from baseline")
	}
	if len(p.UsageSignals) != 0 {
		t.Fatalf("meta-muse-pro usage_signals = %d entries, want 0 (no observed wording)", len(p.UsageSignals))
	}
}

func TestValidateUsageSignals(t *testing.T) {
	good := UsageSignal{ID: "x", Match: `usage limit.*reset at (?P<reset>\S+)`, Kind: UsageSignalKindExhausted, Window: PlanWindow5h, Reset: "reset", ResetLayout: UsageSignalResetClock}

	cases := []struct {
		name   string
		signal UsageSignal
		errSub string
	}{
		{"empty id", UsageSignal{Match: "x", Kind: UsageSignalKindWarning}, "invalid or duplicate usage signal id"},
		{"empty match", UsageSignal{ID: "x", Kind: UsageSignalKindWarning}, "empty match"},
		{"bad regex", UsageSignal{ID: "x", Match: "a(b", Kind: UsageSignalKindWarning}, "bad regexp"},
		{"bad kind", UsageSignal{ID: "x", Match: "a", Kind: "explosion"}, "unknown kind"},
		{"bad window", UsageSignal{ID: "x", Match: "a", Kind: UsageSignalKindWarning, Window: "fortnight"}, "unknown window"},
		{"reset without layout", UsageSignal{ID: "x", Match: "(?P<r>a)", Kind: UsageSignalKindWarning, Reset: "r"}, "reset and reset_layout must be declared together"},
		{"layout without reset", UsageSignal{ID: "x", Match: "a", Kind: UsageSignalKindWarning, ResetLayout: UsageSignalResetEpoch}, "reset and reset_layout must be declared together"},
		{"reset names no group", UsageSignal{ID: "x", Match: "a(?P<r>.)", Kind: UsageSignalKindWarning, Reset: "nope", ResetLayout: UsageSignalResetEpoch}, "reset names no capture group"},
		{"used_percent neither group nor number", UsageSignal{ID: "x", Match: "a", Kind: UsageSignalKindWarning, UsedPercent: "most"}, "neither a capture group nor a number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUsageSignals("rec", []UsageSignal{tc.signal})
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("err=%v want substring %q", err, tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v want nil", err)
			}
		})
	}
	// The duplicate case: two copies of a good signal.
	if err := validateUsageSignals("rec", []UsageSignal{good, good}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("dup err=%v", err)
	}
	// The good signal alone, plus a literal used_percent and a group one, pass.
	lit := good
	lit.ID = "lit"
	lit.UsedPercent = "95"
	lit.Reset, lit.ResetLayout = "", ""
	grp := UsageSignal{ID: "grp", Match: `(?P<u>\d+)% of weekly`, Kind: UsageSignalKindWarning, Window: PlanWindowWeek, UsedPercent: "u"}
	if err := validateUsageSignals("rec", []UsageSignal{good, lit, grp}); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
}

// A plan or tool record that declares a broken signal must fail at load, not
// parse fine and silently never match.
func TestUsageSignalValidationAtLoad(t *testing.T) {
	badPlan := "name: broken\nusage_signals:\n  - id: x\n    match: a(b\n    kind: warning\n"
	if _, err := ParsePlan("broken", []byte(badPlan), nil); err == nil || !strings.Contains(err.Error(), "bad regexp") {
		t.Fatalf("plan err=%v want bad regexp", err)
	}
	badTool := "name: broken\nkind: cli\nusage_signals:\n  - id: x\n    match: a\n    kind: kaboom\n"
	tl, err := ParseTool("broken", []byte(badTool), nil)
	if err != nil {
		t.Fatalf("ParseTool: %v", err)
	}
	if err := tl.ValidateUsageSignals(); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("tool err=%v want unknown kind", err)
	}
}

// Header-only signals: absent header never matches, a non-matching header
// value never matches, and the excerpt is ignored for a header signal.
func TestMatchSignalsHeaderSemantics(t *testing.T) {
	sig := []UsageSignal{{
		ID:          "epoch-reset",
		Header:      "anthropic-ratelimit-unified-reset",
		Match:       `^\s*(?P<reset>\d{9,12})\s*$`,
		Kind:        UsageSignalKindExhausted,
		Reset:       "reset",
		ResetLayout: UsageSignalResetEpoch,
	}}

	if got := MatchSignals(sig, "1791609000", nil, signalsNow); len(got) != 0 {
		t.Fatalf("excerpt must not feed a header signal: %+v", got)
	}
	if got := MatchSignals(sig, "", map[string]string{"anthropic-ratelimit-unified-reset": "soon"}, signalsNow); len(got) != 0 {
		t.Fatalf("non-matching header value must not match: %+v", got)
	}
	got := MatchSignals(sig, "", map[string]string{"ANTHROPIC-RATELIMIT-UNIFIED-RESET": " 1791609000 "}, signalsNow)
	if len(got) != 1 || got[0].ResetAt == nil || !got[0].ResetAt.Equal(time.Unix(1791609000, 0)) {
		t.Fatalf("case-insensitive header lookup: %+v", got)
	}
}

// A declared reset that fails to parse still yields the match with nil reset:
// the vendor saying the limit hit IS the signal; the number is best-effort.
func TestMatchSignalsKeepsUnparseableReset(t *testing.T) {
	sig := []UsageSignal{{
		ID:          "x",
		Match:       `reset at (?P<reset>.+)`,
		Kind:        UsageSignalKindExhausted,
		Reset:       "reset",
		ResetLayout: UsageSignalResetEpoch,
	}}
	got := MatchSignals(sig, "reset at someday", nil, signalsNow)
	if len(got) != 1 || got[0].Signal != "x" || got[0].Kind != UsageSignalKindExhausted || got[0].ResetAt != nil {
		t.Fatalf("got %+v", got)
	}
}

// The reset-layout parsers, one shape each, at the fixed instant.
func TestParseSignalResetLayouts(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		layout string
		want   time.Time
		retry  time.Duration
		ok     bool
	}{
		{"epoch seconds", "1791609000", UsageSignalResetEpoch, time.Unix(1791609000, 0), 0, true},
		{"epoch float", "1791609000.5", UsageSignalResetEpoch, time.Unix(1791609000, int64(500*time.Millisecond)), 0, true},
		{"rfc3339", "2026-06-24T15:04:05Z", UsageSignalResetRFC3339, time.Date(2026, 6, 24, 15, 4, 5, 0, time.UTC), 0, true},
		{"go duration", "3h20m", UsageSignalResetRelative, signalsNow.Add(3*time.Hour + 20*time.Minute), 3*time.Hour + 20*time.Minute, true},
		{"in 45 minutes", "in 45 minutes", UsageSignalResetRelative, signalsNow.Add(45 * time.Minute), 45 * time.Minute, true},
		{"retry-after header", "Retry-After: 120", UsageSignalResetRelative, signalsNow.Add(120 * time.Second), 120 * time.Second, true},
		{"bare seconds", "120", UsageSignalResetRelative, signalsNow.Add(120 * time.Second), 120 * time.Second, true},
		{"clock am", "11:41 AM", UsageSignalResetClock, time.Date(2026, 6, 24, 11, 41, 0, 0, time.UTC), 0, true},
		{"clock rolls to tomorrow", "9:00 AM", UsageSignalResetClock, time.Date(2026, 6, 25, 9, 0, 0, 0, time.UTC), 0, true},
		{"clock 12 pm noon", "12:00 PM", UsageSignalResetClock, time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC), 0, true},
		{"dated with year", "Jul 24th, 2026 9:45 PM", UsageSignalResetClock, time.Date(2026, 7, 24, 21, 45, 0, 0, time.UTC), 0, true},
		{"dated no year rolls forward", "Jan 2nd 9:00 AM", UsageSignalResetClock, time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC), 0, true},
		{"raw go layout", "2026-10-12 17:34:44", "2006-01-02 15:04:05", time.Date(2026, 10, 12, 17, 34, 44, 0, time.UTC), 0, true},
		{"garbage", "when it's ready", UsageSignalResetClock, time.Time{}, 0, false},
		{"empty layout", "2026", "", time.Time{}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, retry, ok := parseSignalReset(tc.value, tc.layout, signalsNow)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if !at.Equal(tc.want) {
				t.Fatalf("reset=%v want %v", at, tc.want)
			}
			if tc.retry > 0 {
				if retry == nil || *retry != tc.retry {
					t.Fatalf("retry=%v want %v", retry, tc.retry)
				}
			} else if retry != nil {
				t.Fatalf("retry=%v want nil", retry)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time             { return &t }
func ptrDuration(d time.Duration) *time.Duration { return &d }
func ptrFloat(f float64) *float64                { return &f }

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func equalDurationPtr(a, b *time.Duration) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
