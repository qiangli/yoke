package fleet

import "testing"

// Sprint: #379; Story: #37; Story-ID: 5b537ed16256
//
// Every API model the baseline ships must declare its context window. genie
// (bashy internal/agentos/genie_external.go) reads Model.ContextLength to size
// its context budget, and a missing value fell back to 32768 tokens, which
// genie turned into a 20480-token budget: glm-5.3, a 1M-context model, lost
// its tool output within a few turns. A subscription model is sized by its own
// harness; an API model is sized by US, so the catalog must say.
func TestEveryBaselineAPIModelDeclaresContextLength(t *testing.T) {
	// The SHIPPED seeds, not the test ring: this is a lint on what we ship.
	// The seeds switch and the path redirects are pinned so a developer's
	// ambient environment can neither drop the seeds nor add a roster.
	t.Setenv("BASHY_FLEET_SEEDS", "")
	t.Setenv("BASHY_MODELS_PATH", "")
	t.Setenv("BASHY_MODELS_DIR", "")
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	c := New(WithoutLocalStore(), WithRoot(t.TempDir()), WithBaselineFS(baselineFS))
	models, errs := c.Models()
	if len(errs) != 0 {
		t.Fatalf("model parse errors: %v", errs)
	}
	api := 0
	for _, m := range models {
		if m.Kind != ModelKindAPI {
			continue
		}
		api++
		if m.ContextLength <= 0 {
			t.Errorf("%s: kind is api but context_length is %d; genie would run it under a 32768-token fallback",
				m.Name, m.ContextLength)
		}
	}
	if api == 0 {
		t.Fatal("no baseline API models")
	}
}
