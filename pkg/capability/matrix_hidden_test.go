package capability

import (
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/fleet"
)

// The matrix seeds from visible tools only: a hidden tool contributes no
// harness row, and agents bound to it get no seeded row either.
func TestSeedSkipsHiddenToolsAndTheirAgents(t *testing.T) {
	root := t.TempDir()
	mk := func() *fleet.Catalog {
		return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	}
	cat := mk()
	if err := cat.SaveTool(fleet.Tool{Name: "loud", Kind: fleet.ToolKindCLI,
		Harness: map[string]float64{"operability": 0.9}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(fleet.Tool{Name: "quiet", Kind: fleet.ToolKindCLI, Hidden: true,
		Harness: map[string]float64{"operability": 0.9}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "m", Kind: fleet.ModelKindLocal}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "loud-agent", Tool: "loud", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "quiet-agent", Tool: "quiet", Model: "m"}); err != nil {
		t.Fatal(err)
	}

	prev := newCatalog
	newCatalog = func() *fleet.Catalog { return mk() }
	t.Cleanup(func() { newCatalog = prev })

	m := seedPriors()
	if _, ok := m.Agents["loud:m"]; !ok {
		t.Errorf("visible binding missing: %v", m.Agents)
	}
	if _, ok := m.Agents["quiet:m"]; ok {
		t.Errorf("agent on a hidden tool must not seed a row: %v", m.Agents)
	}
}
