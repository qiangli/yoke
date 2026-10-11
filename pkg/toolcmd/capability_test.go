package toolcmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/room"
)

type selectionQuotaAdapter struct{ used float64 }

func (selectionQuotaAdapter) Kind() string { return "selection-quota" }
func (a selectionQuotaAdapter) Collect(_ context.Context, _ llmbudget.SourceConfig, now time.Time) (llmbudget.SourceResult, error) {
	return llmbudget.SourceResult{Status: "ok", Metrics: []llmbudget.Metric{{Name: "quota.used_percent", Value: &a.used, Classification: "actual", Source: "test", ObservedAt: now}}}, nil
}

func TestSelectMeasuredCommandSelectsOtherVendorAndPublishesEvent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_CAPABILITY_DIR", dir)
	tools := t.TempDir()
	t.Setenv("BASHY_TOOLS_PATH", tools)
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "vendor-b"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "vendor-b.yaml"), []byte(`name: vendor-b
kind: cli
cli:
  binary: vendor-b
  launch:
    exec: vendor-b {prompt}
commands:
  - name: research
    slash: /research {args}
    mode: print
    capability: deep-research
`), 0o644); err != nil {
		t.Fatal(err)
	}
	matrix := capability.Matrix{SchemaVersion: "bashy-capability-v1", Agents: map[string]map[capability.Capability]capability.Cell{
		"vendor-b:model-b": {capability.CapDeepResearch: {Quality: .9, Source: capability.SourceHost, Samples: 2}},
	}}
	b, err := json.Marshal(matrix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "matrix.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	g := llmbudget.New(llmbudget.Config{Now: func() time.Time { return now }, Adapters: []llmbudget.Adapter{selectionQuotaAdapter{used: 25}}, Policy: &llmbudget.Policy{Version: 1,
		Bindings: []llmbudget.Binding{{Model: "model-b", Agent: "vendor-b:model-b", Provider: "vendor-b", Account: "seat", Pool: "primary", Lane: llmbudget.LaneSubscription}},
		Sources:  []llmbudget.SourceConfig{{ID: "seat", Kind: "selection-quota", Provider: "vendor-b", Account: "seat", Pool: "primary", Lane: llmbudget.LaneSubscription, Enabled: true}},
	}})
	restore := llmbudget.SetDefault(g)
	t.Cleanup(restore)
	var event room.Event
	oldPublish := publishSelection
	publishSelection = func(got room.Event) error { event = got; return nil }
	t.Cleanup(func() { publishSelection = oldPublish })
	tool, command, model, selected, err := SelectMeasuredCommand(context.Background(), "deep-research", "vendor-a", fleet.New(fleet.WithRoot(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name != "vendor-b" || command.Name != "research" || model != "model-b" || selected.Vendor != "vendor-b" || selected.RemainingQuota != .75 {
		t.Fatalf("selection = tool=%+v command=%+v model=%q selected=%+v", tool, command, model, selected)
	}
	if err := recordSelection(selected); err != nil {
		t.Fatal(err)
	}
	if event.Type != room.EventUsage || event.Topic != "capability-selection" || event.Actor != "vendor-b" {
		t.Fatalf("event = %+v", event)
	}
}

func TestSelectMeasuredCommandDoesNotSeedMissingMatrix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_CAPABILITY_DIR", dir)
	_, _, _, _, err := SelectMeasuredCommand(context.Background(), "deep-research", "caller", nil)
	if err == nil {
		t.Fatal("SelectMeasuredCommand unexpectedly selected from an absent matrix")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "matrix.json")); !os.IsNotExist(statErr) {
		t.Fatalf("selection seeded matrix: %v", statErr)
	}
}
