package weave

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// Story 5fec08b11a5f: the assign pool carries the plan tier of the seat each
// model bills through, from fleet plan data; a model with no plan is 0.
func TestAssignPoolCarriesPlanRank(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_MODELS_PATH", "")
	t.Setenv("BASHY_PLANS_PATH", "")
	fleetRoot := t.TempDir()
	cat := fleet.New(fleet.WithRoot(fleetRoot))
	old := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { fleetCatalog = old })
	if err := os.MkdirAll(filepath.Join(fleetRoot, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fleetRoot, "plans", "top-seat.yaml"), []byte("name: top-seat\ntier: max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		cat.SaveTool(fleet.Tool{Name: "tool-a"}),
		cat.SaveModel(fleet.Model{Name: "seated", Band: 3, Kind: fleet.ModelKindSubscription, Plan: "top-seat"}),
		cat.SaveModel(fleet.Model{Name: "unseated", Band: 3}),
		cat.SaveAgent(fleet.Agent{Name: "agent-seated", Tool: "tool-a", Model: "seated", Band: 3}),
		cat.SaveAgent(fleet.Agent{Name: "agent-unseated", Tool: "tool-a", Model: "unseated", Band: 3}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	pool, _, err := sprintAssignPool(t.TempDir(), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, e := range pool {
		got[e.Agent] = e.PlanRank
	}
	if got["agent-seated"] != fleet.PlanTierRank(fleet.PlanTierMax) || got["agent-unseated"] != 0 {
		t.Fatalf("plan ranks = %v", got)
	}
}
