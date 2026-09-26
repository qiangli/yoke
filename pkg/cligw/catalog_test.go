package cligw

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/resolve"
)

func testFleet(t *testing.T) *FleetCatalog {
	t.Helper()
	root := t.TempDir()
	fake := filepath.Join(root, "fake-cli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cat := fleet.New(fleet.WithRoot(filepath.Join(root, "fleet")), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
	tools := []fleet.Tool{
		{Name: "alpha-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: fake, Launch: fleet.ToolLaunch{Exec: fake + " --model {model} {prompt}"}}},
		{Name: "beta-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: fake, Launch: fleet.ToolLaunch{Exec: fake + " --model {model} {prompt}"}}},
		{Name: "dead-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: fake}},
	}
	for _, tool := range tools {
		if err := cat.SaveTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	models := []fleet.Model{
		{Name: "small", Aliases: []string{"swift"}, Family: "small-family", Version: "1", Band: 2, Kind: fleet.ModelKindSubscription, Provider: "anthropic", Capabilities: []string{"completion", "tools"}, Domain: []string{"general"}},
		{Name: "strong", Aliases: []string{"power"}, Band: 4, BandSource: fleet.BandMeasured, Kind: fleet.ModelKindAPI, Provider: "openai", Domain: []string{"coding"}},
		{Name: "frontier", Band: 5, BandSource: fleet.BandDeclared, Kind: fleet.ModelKindSubscription, Provider: "openai", Domain: []string{"reasoning"}},
	}
	for _, model := range models {
		if err := cat.SaveModel(model); err != nil {
			t.Fatal(err)
		}
	}
	agents := []fleet.Agent{
		{Name: "alpha", Aliases: []string{"first"}, Tool: "alpha-tool", Model: "small"},
		{Name: "beta", Tool: "beta-tool", Model: "small"},
		{Name: "gamma", Tool: "beta-tool", Model: "strong"},
		{Name: "omega", Tool: "alpha-tool", Model: "frontier"},
		{Name: "x-cascade", Tool: "alpha-tool", Model: "small", Base: "alpha", Band: 4, BandSource: fleet.BandCascade},
		{Name: "unlaunchable", Tool: "dead-tool", Model: "strong"},
	}
	for _, agent := range agents {
		if err := cat.SaveAgent(agent); err != nil {
			t.Fatal(err)
		}
	}
	return NewFleetCatalog(cat)
}

func TestFleetCatalogRowsAreAgentsAndCascadeBandWins(t *testing.T) {
	cat := testFleet(t)
	rows := cat.Rows(context.Background(), "ignored")
	if len(rows) != 5 {
		t.Fatalf("Rows() returned %d rows, want five launchable agents: %+v", len(rows), rows)
	}
	byBackend := make(map[string]resolve.ModelRow, len(rows))
	for _, row := range rows {
		byBackend[row.Backend] = row
	}
	if got := byBackend["alpha"]; got.Name != "small" || got.Class != 2 {
		t.Fatalf("alpha row = %+v", got)
	}
	if got := byBackend["x-cascade"]; got.Name != "small" || got.Class != 4 {
		t.Fatalf("cascade row = %+v, want model small served at L4", got)
	}
	if _, found := byBackend["unlaunchable"]; found {
		t.Fatal("agent whose tool has no launch contract must be omitted")
	}
	if class, domains, ok := cat.Alias(context.Background(), "swift"); !ok || class != 2 || len(domains) != 1 || domains[0] != "general" {
		t.Fatalf("Alias(swift) = %d, %v, %v", class, domains, ok)
	}
	if _, _, ok := cat.Alias(context.Background(), "L2+"); ok {
		t.Fatal("L2+ cannot be flattened into resolve.Catalog's exact-class alias")
	}
}

func TestModelListOrderingAndBandProvenance(t *testing.T) {
	cat := testFleet(t)
	got := cat.ModelList(context.Background(), Filter{})
	wantPrefix := []string{"L1", "L2", "L3", "L4", "L5", "L1+", "L2+", "L3+", "L4+", "L5+"}
	for i, want := range wantPrefix {
		if got[i].ID != want {
			t.Fatalf("ModelList[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
	byID := make(map[string]ModelEntry, len(got))
	for _, row := range got {
		byID[row.ID] = row
	}
	if row := byID["small"]; row.XBandSource != fleet.BandDeclared {
		t.Fatalf("legacy empty source presented as %q, want declared", row.XBandSource)
	}
	if row := byID["strong"]; row.XBandSource != fleet.BandMeasured {
		t.Fatalf("measured model presented as %q", row.XBandSource)
	}
	if row := byID["x-cascade"]; row.XBand != 4 || row.XBandSource != fleet.BandCascade {
		t.Fatalf("cascade entry = %+v", row)
	}
	if row := byID["alpha"]; row.XWarm != "cold" || row.XQuota != "" {
		t.Fatalf("agent defaults = %+v, want cold and empty quota", row)
	}
	if _, found := byID["unlaunchable"]; found {
		t.Fatal("model list included an agent without a launch contract")
	}
}
