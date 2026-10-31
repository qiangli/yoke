package verify

import "testing"

func TestRepairSurfaceRefusesAddition(t *testing.T) {
	compat := CompareSurface(Surface{"unit:list": true}, Surface{"unit:list": true, "unit:extra": true}, nil)
	if len(compat.Added) != 1 || compat.Added[0] != "unit:extra" {
		t.Fatalf("addition lost: %+v", compat)
	}
}
