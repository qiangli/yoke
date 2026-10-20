package weave

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

// A coach budget nudge injected into a healthy run's log is supervisor
// context, never vendor quota evidence: it must not read as quota-exhausted,
// must not become a quota observation, and must not drive a kill/takeover.
func TestTakeoverCauseCoachNudgeNotQuota(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	tail := "working on the task\nstill making progress\n" + chat.BudgetSteer + "\nback to work\n"
	for _, brief := range []string{"", "Finish the implementation and submit."} {
		got, evidence := weaveTakeoverCause(tail, nil, "a", now, brief)
		if got != "other" || evidence != "" {
			t.Fatalf("brief %q: coach nudge: cause=%q evidence=%q, want cause=%q with no evidence", brief, got, evidence, "other")
		}
	}
	if throttled, signal := weaveClassifyThrottle("claude", 1, tail); throttled {
		t.Fatalf("coach nudge classified as throttle: signal=%q", signal)
	}
}

// Text quoted from the run's own brief is supervisor-provided context, not
// fresh vendor output — even when it names quota words.
func TestTakeoverCauseBriefQuoteNotQuota(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	brief := "Handle the 'usage limit reached' error gracefully and retry."
	tail := "starting work\n" + brief + "\nediting files\n"
	got, _ := weaveTakeoverCause(tail, nil, "a", now, brief)
	if got != "other" {
		t.Fatalf("brief quote: cause=%q, want %q", got, "other")
	}
}

// Real vendor quota evidence must still read as quota-exhausted: vendor
// prose, a vendor usage_limit JSON event, and a rejected rate-limit event.
func TestTakeoverCauseVendorQuotaKept(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rejected := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1791609000,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.23}}}}`
	for _, tc := range []struct{ name, tail string }{
		{"vendor prose", "ERROR: You've hit your usage limit. Try again at 1:30 PM."},
		{"usage_limit json", `{"type":"result","usage_limit":"reset at 13:00"}`},
		{"rejected stream event", rejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := weaveTakeoverCause(tc.tail, nil, "a", now, "")
			if got != "quota-exhausted" {
				t.Fatalf("%s: cause=%q, want %q", tc.name, got, "quota-exhausted")
			}
		})
	}
}

// The same nudge must not become a quota observation on a failed run, while
// genuine vendor evidence alongside an unrelated brief still records.
func TestObservationsFromLogIgnoresCoachNudge(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, steer := range chat.ReflexSteers() {
		tail := "exit 1: something broke\n" + steer + "\n"
		if got := ObservationsFromLog("claude", "fixture-model", tail, now, true, 1, ""); len(got) != 0 {
			t.Fatalf("steer observation: %+v", got)
		}
	}
	vendor := "before\nERROR: You've hit your usage limit. Try again at 1:30 PM.\nafter"
	if got := ObservationsFromLog("codex", "fixture-model", vendor, now, true, 1, "Finish the implementation and submit."); len(got) != 1 {
		t.Fatalf("vendor evidence with brief: %+v", got)
	}
}
