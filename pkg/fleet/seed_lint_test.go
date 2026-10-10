package fleet

import (
	"strings"
	"testing"
)

// TestSeedLint is the curation rule for the compiled-in fleet (plan §E): a
// non-retired agent seed is L3 or better and binds a curated tool and model,
// meaning one that is built in, not retired and not hidden. Anything outside
// that list is a retired built-in for one release, then a custom definition
// in bashy's examples/fleet. bashy's scripts/check-seed-bands.sh runs this
// test at release time.
func TestSeedLint(t *testing.T) {
	for _, k := range []string{"BASHY_FLEET_SEEDS", "BASHY_TOOLS_PATH", "BASHY_MODELS_PATH", "BASHY_AGENTS_PATH"} {
		t.Setenv(k, "")
	}
	cat := New(WithBaselineFS(baselineFS), WithoutLocalStore(), WithoutCloudOverlay())
	agents, errs := cat.Agents()
	for _, err := range errs {
		t.Errorf("seed parse: %v", err)
	}
	var bad []string
	for _, a := range agents {
		if a.IsRetired() {
			continue
		}
		var why []string
		tool, ok := cat.Tool(a.Tool)
		switch {
		case !ok:
			why = append(why, "tool "+a.Tool+" is not built in")
		case tool.IsRetired():
			why = append(why, "tool "+a.Tool+" is retired")
		case tool.Hidden:
			why = append(why, "tool "+a.Tool+" is hidden")
		}
		band := a.Band
		if a.Model != "" {
			m, ok := cat.Model(a.Model)
			switch {
			case !ok:
				why = append(why, "model "+a.Model+" is not built in")
			case m.IsRetired():
				why = append(why, "model "+a.Model+" is retired")
			}
			if band == 0 {
				band = m.Band
			}
		}
		if band < 3 {
			why = append(why, "band below L3")
		}
		if len(why) > 0 {
			bad = append(bad, a.Name+": "+strings.Join(why, ", "))
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d agent seed(s) break the curation rule (retire them or fix the binding):\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}
