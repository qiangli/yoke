package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnmeasuredProbeDocs verifies that plans without a measured probe
// document why they stay unmeasured (Story #1025). The probe seeds for
// google-ai-ultra (agy), zai-glm-coding-pro (z.ai GLM) and meta-muse-pro
// (muse) were investigated on 2026-10-17 and found to have no documented
// read-only source; each plan yaml must therefore carry an explicit
// "stays unmeasured" rationale so quota remains unmeasured instead of
// inventing 0% or a fabricated percentage.
func TestUnmeasuredProbeDocs(t *testing.T) {
	cases := []struct {
		plan string
		must []string
	}{
		{
			plan: "google-ai-ultra",
			must: []string{
				"stays unmeasured",
				"agy",
				"~/.gemini",
			},
		},
		{
			plan: "zai-glm-coding-pro",
			must: []string{
				"stays unmeasured",
				"docs.z.ai",
			},
		},
		{
			plan: "meta-muse-pro",
			must: []string{
				"stays unmeasured",
				"muse",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.plan, func(t *testing.T) {
			// Read the baseline file directly so the test does not depend on
			// catalog loading and works in any BASHY_FLEET_DIR overlay.
			p := filepath.Join("baseline", "plans", tc.plan+".yaml")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			s := string(b)
			for _, needle := range tc.must {
				if !strings.Contains(strings.ToLower(s), strings.ToLower(needle)) {
					t.Fatalf("%s: plan yaml missing %q.\nGot:\n%s", tc.plan, needle, s)
				}
			}
			// Must not carry a probe; the documented reason is the artifact,
			// not a fabricated metric.
			c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
			// reload from baselineFS via New without local overlay: use baseline directly
			// Re-parse via catalog from default (WithoutCloudOverlay still loads baselineFS)
			c2 := New()
			if pl, ok := c2.Plan(tc.plan); ok && len(pl.Usage) != 0 {
				t.Fatalf("%s unexpectedly carries usage probes: %+v", tc.plan, pl.Usage)
			}
			_ = c
		})
	}
}
