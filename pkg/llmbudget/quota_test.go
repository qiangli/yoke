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
	cat := fleet.New()
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
