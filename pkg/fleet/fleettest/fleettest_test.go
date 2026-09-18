package fleettest_test

import (
	"testing"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// Ring must reach the catalog through the shared-dir ring with nothing
// dangling, over a local store that is the fenced scratch root and not the
// operator's.
func TestRingMountsModelsAndAgentsOverAFencedStore(t *testing.T) {
	root := fleettest.Ring(t)
	c := fleet.New()
	if c.Root() != root {
		t.Fatalf("local store root = %q, want the fenced %q", c.Root(), root)
	}
	models, errs := c.Models()
	if len(errs) != 0 || len(models) == 0 {
		t.Fatalf("models = %d entries, errs = %v", len(models), errs)
	}
	agents, errs := c.Agents()
	if len(errs) != 0 || len(agents) == 0 {
		t.Fatalf("agents = %d entries, errs = %v", len(agents), errs)
	}
	for _, m := range models {
		if m.Ring != assetring.RingShared {
			t.Fatalf("model %s came from ring %v, want the shared ring", m.Name, m.Ring)
		}
	}
	for _, a := range agents {
		if _, _, _, err := c.Binding(a.Name); err != nil {
			t.Errorf("%s: %v", a.Name, err)
		}
	}
}
