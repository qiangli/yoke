package cligw

import (
	"context"
	"github.com/qiangli/yoke/pkg/fleet"
	"testing"
)

func TestRetirementRemovesRoutingCandidates(t *testing.T) {
	for _, tc := range []struct{ kind, name string }{{fleet.KindTool, "alpha-tool"}, {fleet.KindModel, "small"}, {fleet.KindAgent, "alpha"}} {
		t.Run(tc.kind, func(t *testing.T) {
			cat := testFleet(t)
			if err := cat.Registry().Retire(tc.kind, tc.name, "", "next"); err != nil {
				t.Fatal(err)
			}
			// Use a fresh catalog snapshot as a new routing request would after refresh.
			cat = NewFleetCatalog(cat.Registry())
			for _, a := range cat.inventory() {
				if a.Name == "alpha" {
					t.Fatal("retired candidate remains")
				}
			}
			a, ok := cat.Registry().Agent("alpha")
			if !ok {
				t.Fatal("historical agent lost")
			}
			if tc.kind != fleet.KindAgent && (a.IsRetired() || a.Unavailable == "") {
				t.Fatalf("dependent agent: %+v", a)
			}
			for _, row := range cat.Rows(context.Background(), "") {
				if row.Backend == "alpha" {
					t.Fatal("retired candidate advertised")
				}
			}
		})
	}
}
