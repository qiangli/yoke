package chat

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestPTYCoachPinnedCatalogStaysReflexOnly(t *testing.T) {
	pinned := newPTYCoach("fixture", "", fleet.New())
	if pinned.escalate != nil {
		t.Fatal("pinned catalog enabled host-catalog escalation")
	}
	if host := newPTYCoach("fixture", "", nil); host.escalate == nil {
		t.Fatal("host catalog lost graduated escalation")
	}
}
