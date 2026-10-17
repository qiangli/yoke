package llmbudget

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TestQuotaUnmeasuredStaysNotFabricated verifies Story #1025: the three
// investigated seats (google-ai-pro/agy, zai-glm-coding-pro/glm, meta-muse-pro/muse)
// have no measured probe, so quota must report unmeasured with no UsedPercent
// — never 0% and never a fabricated percentage. The plan yamls document why
// each stays unmeasured (checked 2026-10-17).
func TestQuotaUnmeasuredStaysNotFabricated(t *testing.T) {
	isolateReportCatalog(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	models := []fleet.Model{
		{Name: "agy-model", Provider: "gemini", Plan: "google-ai-pro", Family: "gemini"},
		{Name: "glm-model", Provider: "openai-compat", Plan: "zai-glm-coding-pro", Family: "glm"},
		{Name: "muse-model", Provider: "meta", Plan: "meta-muse-pro", Family: "muse"},
	}
	plans := []fleet.Plan{
		{Name: "google-ai-pro", Label: "agy"},
		{Name: "zai-glm-coding-pro", Label: "glm"},
		{Name: "meta-muse-pro", Label: "muse"},
	}
	agents := []fleet.Agent{
		{Model: "agy-model", Tool: "agy"},
		{Model: "glm-model", Tool: "glm"},
		{Model: "muse-model", Tool: "muse"},
	}
	report := &Report{}
	rows := quotaRows(report, models, plans, agents, &Policy{}, QuotaOptions{}, now)
	want := map[string]bool{"google-ai-pro": false, "zai-glm-coding-pro": false, "meta-muse-pro": false}
	for _, r := range rows {
		if _, ok := want[r.Account]; !ok {
			continue
		}
		want[r.Account] = true
		if r.Status != "unmeasured" {
			t.Fatalf("%s status = %q, want unmeasured (never 0%%)", r.Account, r.Status)
		}
		if r.UsedPercent != nil {
			t.Fatalf("%s UsedPercent = %v, want nil (fabricated)", r.Account, *r.UsedPercent)
		}
		if r.Classification == "measured" || r.Classification == "actual" {
			t.Fatalf("%s classification = %q, want not measured", r.Account, r.Classification)
		}
		// Explicitly ensure no zero-percent row is emitted.
		if r.UsedPercent != nil && *r.UsedPercent == 0 {
			t.Fatalf("%s leaked 0%%", r.Account)
		}
	}
	for acct, found := range want {
		if !found {
			t.Fatalf("missing unmeasured row for %s: %+v", acct, rows)
		}
	}
}
