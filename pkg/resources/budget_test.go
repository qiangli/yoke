package resources

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmbudget"
)

func fixedGate(t *testing.T, now time.Time) {
	t.Helper()
	g := llmbudget.New(llmbudget.Config{
		Now: func() time.Time { return now },
		Models: map[string]llmbudget.Model{
			"glm": {Name: "glm", Kind: fleet.ModelKindSubscription, Provider: "zai", Plan: "glm-pro",
				Limits: llmbudget.Limits{DailyTokens: 1000}},
			// No limit metadata — the case the fleet logs in production.
			"claude-sub": {Name: "claude-sub", Kind: fleet.ModelKindSubscription, Provider: "anthropic", Plan: "max"},
		},
	})
	g.Record("glm", 100, 50, 0)
	g.Record("claude-sub", 300, 200, 0)
	t.Cleanup(llmbudget.SetDefault(g))
}

func TestCollectBudgetEnvelope(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	fixedGate(t, now)

	br := CollectBudget(now, nil)
	if br.SchemaVersion != SchemaVersion || br.Kind != "budget" {
		t.Fatalf("envelope: got %q/%q, want %q/budget", br.SchemaVersion, br.Kind, SchemaVersion)
	}
	if len(br.Models) != 2 {
		t.Fatalf("want both recorded models, got %+v", br.Models)
	}
	if br.LimitsKnown != 1 {
		t.Fatalf("limits_known: got %d, want 1", br.LimitsKnown)
	}

	byName := map[string]llmbudget.BudgetStatus{}
	for _, m := range br.Models {
		byName[m.Model] = m
	}
	if s := byName["claude-sub"]; s.LimitKnown || s.Limit != nil || s.Remaining != nil {
		t.Fatalf("no-limit model must report UNKNOWN, got %+v", s)
	}
	if s := byName["glm"]; !s.LimitKnown || *s.Limit != 1000 || *s.Remaining != 850 || s.Spent != 150 {
		t.Fatalf("known-limit model: got %+v", s)
	}

	// The wire shape a renderer sees: no limit key at all for the unknown row.
	b, err := json.Marshal(br)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, m := range wire.Models {
		if m["model"] != "claude-sub" {
			continue
		}
		if _, ok := m["limit"]; ok {
			t.Fatalf("unknown limit must not appear on the wire: %s", b)
		}
		if known, ok := m["limit_known"].(bool); !ok || known {
			t.Fatalf("limit_known must be present and false: %s", b)
		}
	}
}

func TestFormatBudgetTableSaysUnknown(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	fixedGate(t, now)

	out := FormatBudgetTable(CollectBudget(now, []string{"claude-sub"}))

	// Check the DATA ROW, not the whole report: the footer legitimately uses
	// the word "unlimited" to explain what unknown is not.
	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "claude-sub") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("no row for claude-sub in:\n%s", out)
	}
	fields := strings.Fields(row)
	limit, remaining := fields[len(fields)-2], fields[len(fields)-1]
	if limit != "unknown" || remaining != "unknown" {
		t.Fatalf("missing ceiling must render as unknown, got limit=%q remaining=%q in:\n%s", limit, remaining, out)
	}
	for _, bad := range []string{"0", "unlimited", "∞", "-1"} {
		if limit == bad || remaining == bad {
			t.Fatalf("unknown must not render as %q, got:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "neither exhausted nor unlimited") {
		t.Fatalf("report must call out the unknown rows, got:\n%s", out)
	}
}
