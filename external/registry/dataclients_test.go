package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/dataclients"
)

func TestDataClientEntries(t *testing.T) {
	for _, n := range dataclients.Names() {
		e, ok := Lookup(n)
		if !ok {
			t.Fatalf("%s not registered", n)
		}
		c, _ := dataclients.Lookup(n)
		if e.Tier != 6 || e.License != c.License || !e.PreferHost || e.EnvVersion != "" {
			t.Errorf("%s: wrong entry %+v", n, e)
		}
		if _, err := e.Resolve(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "no sha256-pinned managed release") {
			t.Errorf("%s: Resolve must refuse auto-provisioning: %v", n, err)
		}
		if !strings.Contains(e.Long, c.RemoteLicense) {
			t.Errorf("%s: help must state the remote engine licence separately", n)
		}
		if cmd := e.NewCmd(); cmd.Name() != n || !cmd.DisableFlagParsing {
			t.Errorf("%s: NewCmd wrong", n)
		}
	}
}
