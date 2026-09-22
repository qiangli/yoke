package board

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

func TestClaimsPanelShowsNamedHoldsWithHonestHostState(t *testing.T) {
	now := time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)
	holder := principal.Ref{Name: "lintel", Host: "dragon"}
	b := &Board{GeneratedAt: now, Claims: []*coord.Claim{
		{Resource: "do1", Mode: coord.ModeLease, Holder: holder, Intent: "leaf replay",
			AcquiredAt: now.Add(-5 * time.Minute), Heartbeat: now.Add(-time.Minute)},
		{Resource: "do2", Mode: coord.ModeLease, Holder: holder, Intent: "old work",
			AcquiredAt: now.Add(-coord.TTL - 2*time.Minute), Heartbeat: now.Add(-coord.TTL - time.Minute)},
		{Resource: "mb1", Mode: coord.ModeLease, Holder: holder, Intent: "inspect",
			AcquiredAt: now.Add(-time.Minute)},
		{Project: "bashy", Roots: []string{"/w/bashy"}, Holder: holder,
			AcquiredAt: now.Add(-time.Minute), Heartbeat: now},
	}}
	v := claimsPanel().Build(b)
	if v.ID != "claims" || v.Title != "Claims" || len(v.Rows) != 3 {
		t.Fatalf("panel = %#v", v)
	}
	wants := []string{"live on dragon", "lapsed on dragon", "unknown on dragon"}
	for i, want := range wants {
		if got := v.Rows[i][2]; got != want {
			t.Errorf("row %d state = %q, want %q", i, got, want)
		}
	}
}

func TestClaimsPanelEmptyState(t *testing.T) {
	v := claimsPanel().Build(&Board{GeneratedAt: time.Now()})
	if len(v.Rows) != 0 || v.Collapsed != "0 named hold(s) recorded on this host" {
		t.Fatalf("empty panel = %#v", v)
	}
}
