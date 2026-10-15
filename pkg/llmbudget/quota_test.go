package llmbudget

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestQuotaGroupingWorkersAndDoor(t *testing.T) {
	isolateReportCatalog(t)

	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cat := fleet.New()
	for _, err := range []error{
		cat.SaveModel(fleet.Model{Name: "quota-fixture", Provider: "fixture-vendor", Plan: "seat", Kind: fleet.ModelKindSubscription}),
		cat.SaveTool(fleet.Tool{Name: "quota-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: binary}}),
		cat.SaveAgent(fleet.Agent{Name: "quota-agent", Tool: "quota-tool", Model: "quota-fixture"}),
		cat.SaveModel(fleet.Model{Name: "quota-local", Provider: "ollama", Kind: fleet.ModelKindLocal}),
		cat.SaveAgent(fleet.Agent{Name: "quota-local-agent", Tool: "quota-tool", Model: "quota-local"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	used := 25.0
	balance := 8.0
	policy := &Policy{Version: 1, Constraints: []Constraint{{Provider: "fixture-vendor", Concurrency: ptrInt(8)}}, Sources: []SourceConfig{{ID: "fixture", Kind: "headroom-fixture", Provider: "fixture-vendor", Account: "seat", Pool: "seat", Lane: LaneSubscription, Enabled: true}}}
	adapter := headroomAdapter{metrics: []Metric{{Name: "quota.used_percent", Value: &used, Window: "5h", Classification: "measured", Source: "fixture", ObservedAt: now}, {Name: "usage.balance", Value: &balance, Window: "5h", Unit: "credits", Classification: "measured", Source: "fixture", ObservedAt: now}}}
	g := New(Config{Policy: policy, Adapters: []Adapter{adapter}, Now: func() time.Time { return now }, Models: map[string]Model{}})
	rows, e := g.Quota(context.Background(), QuotaOptions{Provider: "fixture-vendor", Now: now})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, r := range rows {
		if r.Provider == "fixture-vendor" && r.Window == "5h" {
			found = true
			if r.Headroom == nil || math.Abs(*r.Headroom-.75) > 1e-9 || r.SuggestedWorkers == nil || *r.SuggestedWorkers != 6 || r.Balance == nil || *r.Balance != 8 {
				t.Fatalf("row %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("missing grouped row %+v", rows)
	}
	*policy.Constraints[0].Concurrency = 0
	rows, e = g.Quota(context.Background(), QuotaOptions{Provider: "fixture-vendor", Now: now})
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range rows {
		if r.Provider == "fixture-vendor" && r.Window == "5h" {
			if r.SuggestedWorkers == nil || *r.SuggestedWorkers != 0 {
				t.Fatalf("zero concurrency %+v", r)
			}
		}
	}
	if e := cat.SaveModel(fleet.Model{Name: "quota-inactive", Provider: "inactive-vendor", Kind: fleet.ModelKindAPI, APIKeyRef: "no-such-ref"}); e != nil {
		t.Fatal(e)
	}
	rows, e = g.Quota(context.Background(), QuotaOptions{Provider: "inactive-vendor", All: true, Now: now})
	if e != nil || len(rows) != 1 || rows[0].Status != "not-configured" {
		t.Fatalf("inactive %+v %v", rows, e)
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	if e := os.MkdirAll(filepath.Join(home, "broker"), 0700); e != nil {
		t.Fatal(e)
	}
	audit := fmt.Sprintf("{\"time\":%q,\"prompt_tokens\":12,\"completion_tokens\":3}\n{\"time\":%q,\"prompt_tokens\":100}\n", now.Add(-time.Hour).Format(time.RFC3339), now.Add(-6*time.Hour).Format(time.RFC3339))
	if e := os.WriteFile(filepath.Join(home, "broker", "audit.jsonl"), []byte(audit), 0600); e != nil {
		t.Fatal(e)
	}
	rows, e = g.Quota(context.Background(), QuotaOptions{Provider: "local", Now: now})
	if e != nil {
		t.Fatal(e)
	}
	found = false
	for _, r := range rows {
		if r.Source == "bashy-door" {
			found = true
			if r.Tokens == nil || *r.Tokens != 15 {
				t.Fatalf("door %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("missing door %+v", rows)
	}
}
func ptrInt(v int) *int { return &v }

func TestQuotaBillingAccounts(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	models := []fleet.Model{
		{Name: "claude-model", Provider: "anthropic", Plan: "claude-max", Family: "claude"},
		{Name: "codex-model", Provider: "openai", Plan: "chatgpt-pro", Family: "gpt"},
		{Name: "agy-model", Provider: "gemini", Plan: "google-ai-pro", Family: "gemini"},
		{Name: "muse-model", Provider: "meta", Plan: "meta-muse-pro", Family: "muse"},
		{Name: "glm-model", Provider: "openai-compat", Plan: "zai-glm-coding-pro", Family: "glm"},
		{Name: "deepseek-model", Provider: "openai-compat", APIKeyRef: "deepseek-ref", Family: "deepseek"},
		{Name: "kimi-model", Provider: "openai-compat", APIKeyRef: "moonshot-ref", Family: "kimi"},
		{Name: "ollama-model", Provider: "ollama", Kind: fleet.ModelKindLocal},
		{Name: "none-model", Provider: "none", APIKeyRef: "noise"},
		{Name: "unknown-model", Provider: "unknown"},
		{Name: "unused-model", Provider: "openai-compat", APIKeyRef: "unused-key", Family: "unused"},
	}
	plans := []fleet.Plan{
		{Name: "claude-max"}, {Name: "chatgpt-pro"}, {Name: "google-ai-pro"}, {Name: "meta-muse-pro"},
		{Name: "zai-glm-coding-pro", Limits: []fleet.PlanLimit{{Window: "5h", Unit: "credits", Value: 12000}}},
		{Name: "unused-plan", Label: "explicit"},
	}
	agents := []fleet.Agent{}
	for i, tool := range []string{"claude", "codex", "agy", "muse", "one", "one", "one", "ollama"} {
		agents = append(agents, fleet.Agent{Model: models[i].Name, Tool: tool})
		if i >= 4 && i <= 6 {
			agents = append(agents, fleet.Agent{Model: models[i].Name, Tool: "two"})
		}
	}
	zero := 0.0
	report := &Report{}
	for _, m := range models {
		report.Accounts = append(report.Accounts, AccountReport{Account: quotaAccount(m), Provider: m.Provider, Metrics: []Metric{{Name: "usage.tokens", Source: "local-meter", Value: &zero, Classification: "actual"}}})
	}
	for _, tc := range []struct {
		name  string
		all   bool
		count int
	}{{"configured", false, 8}, {"all", true, 10}} {
		t.Run(tc.name, func(t *testing.T) {
			rows := quotaRows(report, models, plans, agents, &Policy{}, QuotaOptions{All: tc.all}, time.Now())
			if len(rows) != tc.count {
				t.Fatalf("rows = %+v", rows)
			}
			labels := map[string]bool{}
			for _, r := range rows {
				labels[r.Label] = true
				if r.Account == "" || r.Provider == "none" || r.Provider == "unknown" {
					t.Fatalf("noise %+v", r)
				}
				if r.Tokens != nil || r.ObservedAt != nil || r.Classification != "" || r.UsedPercent != nil {
					t.Fatalf("invented measurement %+v", r)
				}
				want := "unmeasured"
				if !r.Configured {
					want = "not-configured"
				}
				if r.Status != want {
					t.Fatalf("status %+v", r)
				}
				if r.Label == "glm" && (r.Limit != "12000 credits/5h" || r.Window != "5h" || r.Source != "plan") {
					t.Fatalf("limit %+v", r)
				}
			}
			for _, label := range []string{"claude", "codex", "agy", "muse", "glm", "deepseek", "kimi", "local"} {
				if !labels[label] {
					t.Errorf("missing %s", label)
				}
			}
			if tc.all && (!labels["explicit"] || !labels["unused"]) {
				t.Fatal(labels)
			}
		})
	}
	// Explicit label beats a single tool; mixed families fall back to account.
	plans[0].Label = "operator"
	models = append(models, fleet.Model{Name: "other", Provider: "openai-compat", APIKeyRef: "deepseek-ref", Family: "different"})
	rows := quotaRows(report, models, plans, agents, &Policy{}, QuotaOptions{}, time.Now())
	labels := map[string]bool{}
	for _, r := range rows {
		labels[r.Label] = true
	}
	if !labels["operator"] || !labels["deepseek-ref"] {
		t.Fatal(labels)
	}
	// Separate measured windows share account identity; wire-compatible accounts do not.
	in, out, percent := 10.0, 5.0, 25.0
	report.Accounts = []AccountReport{
		{Account: "deepseek-ref", Metrics: []Metric{{Name: "usage.tokens_in", Source: "probe", Value: &in, Window: "5h"}, {Name: "usage.tokens_out", Source: "probe", Value: &out, Window: "5h"}, {Name: "quota.used_percent", Source: "probe", Value: &percent, Window: "week", Classification: "measured"}}},
		{Account: "moonshot-ref", Metrics: []Metric{{Name: "usage.tokens_in", Source: "probe", Value: &out, Window: "5h"}}},
	}
	rows = quotaRows(report, models, plans, agents, &Policy{}, QuotaOptions{}, time.Now())
	if len(rows) != 9 {
		t.Fatal(rows)
	}
	for _, r := range rows {
		if r.Account == "deepseek-ref" && r.Window == "5h" && (r.Tokens == nil || *r.Tokens != 15) {
			t.Fatal(r)
		}
		if r.Account == "moonshot-ref" && (r.Tokens == nil || *r.Tokens != 5) {
			t.Fatal(r)
		}
	}
}
