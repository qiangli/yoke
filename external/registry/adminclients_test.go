package registry

import "testing"

func TestAdminClientEntries(t *testing.T) {
	for name, lic := range map[string]string{"m365": "MIT", "gam": "Apache-2.0"} {
		e, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if e.Tier != 6 || e.License != lic || !e.PreferHost || e.Resolve == nil {
			t.Errorf("%s: bad entry tier=%d license=%q preferHost=%v", name, e.Tier, e.License, e.PreferHost)
		}
		if c := e.NewCmd(); c.Name() != name || !c.DisableFlagParsing {
			t.Errorf("%s: bad command", name)
		}
		if _, err := e.Resolve(nil, ""); err == nil { //nolint:staticcheck // host-only never touches ctx
			t.Errorf("%s must be host-only", name)
		}
	}
}
