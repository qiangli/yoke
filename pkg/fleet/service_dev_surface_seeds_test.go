package fleet

import "testing"

// A paired developer host needs both human-facing front doors after an
// upgrade. These seeds are the desired state when no operator overlay exists;
// seeding either disabled strands sprint and meeting access until someone is
// already on the host to repair it.
func TestDeveloperSurfaceSeedsAreEnabled(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)
	for _, name := range []string{"apps", "meet"} {
		s, ok := cat.Service(name)
		if !ok {
			t.Errorf("%s seed missing", name)
			continue
		}
		if !s.Enabled {
			t.Errorf("%s seed disabled; paired hosts need this front door by default", name)
		}
		if s.ProviderName() != ServiceProviderProcess {
			t.Errorf("%s provider = %q, want process", name, s.ProviderName())
		}
	}
}

func TestDeveloperSurfaceSeedsRespectOperatorDisable(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	for _, name := range []string{"apps", "meet"} {
		mustSvc(t, opts, "", "disable", name)
		if s, _ := New(opts...).Service(name); s.Enabled {
			t.Errorf("%s still enabled after operator disable", name)
		}
	}
}
