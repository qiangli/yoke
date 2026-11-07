package sota

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// isolatedSotaCatalog pins the synthesis default to an embedded-baseline
// catalog rooted at an empty temp dir: no developer-local or cloud overlay.
func isolatedSotaCatalog(t *testing.T) *fleet.Catalog {
	t.Helper()
	t.Setenv("BASHY_ROSTERS_PATH", "")
	return fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
}

func TestSotaDefaultAgentFromSeededRoster(t *testing.T) {
	prev := sotaCatalog
	sotaCatalog = func() *fleet.Catalog { return isolatedSotaCatalog(t) }
	defer func() { sotaCatalog = prev }()
	if got := sotaDefaultAgent(); got != "claude" {
		t.Fatalf("sotaDefaultAgent=%q, want claude (seeded roles/conductor)", got)
	}
}

func TestSotaDefaultAgentSeedsOffFallsBack(t *testing.T) {
	t.Setenv(fleet.SeedsEnv, "off")
	prev := sotaCatalog
	sotaCatalog = func() *fleet.Catalog { return isolatedSotaCatalog(t) }
	defer func() { sotaCatalog = prev }()
	if got := sotaDefaultAgent(); got != sotaAgentFallback {
		t.Fatalf("sotaDefaultAgent=%q, want fallback %q with seeds off", got, sotaAgentFallback)
	}
}
