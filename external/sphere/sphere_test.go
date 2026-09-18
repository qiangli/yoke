package sphere

import "testing"

func TestSubVerbs(t *testing.T) {
	for _, v := range []string{"mesh", "shard", "pool", "peers"} {
		if _, ok := subVerbs[v]; !ok {
			t.Errorf("sphere is missing sub-verb %q", v)
		}
	}
	if _, ok := subVerbs["cluster"]; ok {
		t.Error("cluster is tier 5 — must not be a sphere sub-verb")
	}
}

func TestResolveOutpostReExport(t *testing.T) {
	// Delegates to meshagent.Resolve; a clean env with no outpost resolves false.
	t.Setenv("OUTPOST_BIN", "")
	// (Can't assert absence portably since a host may have outpost on PATH; just
	// ensure the call is wired and returns without panic.)
	_, _ = ResolveOutpost()
}
