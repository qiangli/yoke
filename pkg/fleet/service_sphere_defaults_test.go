package fleet

import "testing"

// Sprint 436 (story 1b4773cfbdd7): Sprint 418 S18 retired ollama_enabled,
// ollama_pool_enabled and the shard block with no compatibility reader, and
// the replacement baseline seeds all declared enabled: false — so an
// upgraded host silently stopped the ollama proxy, the pool push, and the
// mesh/peer-plane/shard formation. A freshly paired host must run all three
// out of the box: the seeds below are the desired state when no operator
// overlay exists. Pairing itself stays the runtime gate — outpost brings the
// mesh and peer plane up for an enabled shard unit and refuses shard setup
// on an unpaired host — so an enabled seed on an unpaired host starts
// nothing.

// A fresh host (no overlays) runs local inference and shard formation.
func TestSphereInferenceSeedsAreEnabled(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)
	for _, name := range []string{"ollama", "ollama-pool", "shard"} {
		s, ok := cat.Service(name)
		if !ok {
			t.Errorf("%s seed missing", name)
			continue
		}
		if !s.Enabled {
			t.Errorf("%s seed disabled; a freshly paired host needs this unit by default", name)
		}
		if s.ProviderName() != ServiceProviderInternal || s.Owner != ServiceOwnerOutpost {
			t.Errorf("%s = provider %q owner %q, want internal owned by outpost",
				name, s.ProviderName(), s.Owner)
		}
	}
}

// An explicit operator disable survives the restored default: the overlay
// wins over the seed, and re-enabling returns to the default.
func TestSphereInferenceSeedsRespectOperatorDisable(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	for _, name := range []string{"ollama", "ollama-pool", "shard"} {
		mustSvc(t, opts, "", "disable", name)
		if s, _ := New(opts...).Service(name); s.Enabled {
			t.Errorf("%s still enabled after operator disable", name)
		}
	}
	for _, name := range []string{"ollama", "ollama-pool", "shard"} {
		mustSvc(t, opts, "", "enable", name)
		if s, _ := New(opts...).Service(name); !s.Enabled {
			t.Errorf("%s still disabled after operator re-enable", name)
		}
	}
}
