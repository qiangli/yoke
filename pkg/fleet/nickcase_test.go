package fleet_test

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TestAgentNameCaseInsensitive — a human types a nickname the way it reads
// (`elif`), not the way it is stored (`Elif`). Agent() falls back to a
// case-insensitive match only after every exact match has failed, so it never
// shadows a precise binding.
func TestAgentNameCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI,
		Harness: map[string]float64{"operability": 0.95}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "opus", Quality: 0.92, CostMicro: 15000}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "bond-agent", Tool: "claude", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"bond-agent", "BOND-AGENT", "Bond-Agent"} {
		a, ok := cat.Agent(q)
		if !ok || a.Name != "bond-agent" {
			t.Errorf("Agent(%q) = (%q, %v); want (bond-agent, true)", q, a.Name, ok)
		}
	}
	if _, ok := cat.Agent("no-such-agent"); ok {
		t.Error("Agent(no-such-agent) should not resolve")
	}
}
