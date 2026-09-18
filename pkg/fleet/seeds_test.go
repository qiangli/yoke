package fleet

import (
	"testing"

	"github.com/qiangli/yoke/pkg/assetring"
)

// The embedded ring ships SEEDS: a default model + agent roster with band
// pegs, resolvable with nothing mounted above it. This is what a process that
// does not inherit the operator's shell exports — a headless worker, a daemon,
// a fresh host — routes on; before the seeds it saw an empty fleet and every
// band picker failed closed. TestMain strips the ring env, so only the
// compiled-in baseline is under test here.
func TestEmbeddedSeedsResolve(t *testing.T) {
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())

	models, errs := c.Models()
	if len(errs) != 0 {
		t.Fatalf("seed models parse: %v", errs)
	}
	if len(models) < 27 {
		t.Fatalf("seed models = %d, want >= 27", len(models))
	}
	for _, m := range models {
		if m.Ring != assetring.RingEmbedded {
			t.Errorf("model %q ring = %v, want embedded", m.Name, m.Ring)
		}
		if m.Band < 1 || m.Band > MaxBand {
			t.Errorf("model %q band = %d, want 1..%d — an unpegged seed cannot be routed", m.Name, m.Band, MaxBand)
		}
	}

	agents, errs := c.Agents()
	if len(errs) != 0 {
		t.Fatalf("seed agents parse: %v", errs)
	}
	if len(agents) < 39 {
		t.Fatalf("seed agents = %d, want >= 39", len(agents))
	}
	for _, a := range agents {
		if _, _, _, err := c.Binding(a.Name); err != nil {
			t.Errorf("seed agent %q does not resolve against the embedded ring alone: %v", a.Name, err)
		}
	}

	// The family aliases are derived at load (highest version wins, a tie
	// fails closed); every family the seeds ship must resolve to one model.
	for _, alias := range []string{"opus", "sonnet", "haiku", "fable", "gpt", "gemini", "gemini-flash", "deepseek", "glm", "kimi", "kimi-code"} {
		if _, ok := c.Model(alias); !ok {
			t.Errorf("family alias %q does not resolve — a seed family is missing or its versions tie", alias)
		}
	}
}

// BASHY_FLEET_SEEDS=off drops the seeded roster and nothing else: the tool
// launch contracts are the mechanism and stay embedded. This is the switch
// fleettest.Ring relies on, and what an org that publishes its own catalog
// sets on its hosts.
func TestSeedsSwitchDropsRosterNotContracts(t *testing.T) {
	t.Setenv(SeedsEnv, "off")
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
	if models, _ := c.Models(); len(models) != 0 {
		t.Fatalf("seeds off: models = %d, want 0", len(models))
	}
	if agents, _ := c.Agents(); len(agents) != 0 {
		t.Fatalf("seeds off: agents = %d, want 0", len(agents))
	}
	tools, errs := c.Tools(true)
	if len(errs) != 0 || len(tools) == 0 {
		t.Fatalf("seeds off must keep the tool launch contracts: %d tools, errs %v", len(tools), errs)
	}
}
