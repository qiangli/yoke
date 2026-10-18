package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/msgclients"
)

func TestMsgClientEntries(t *testing.T) {
	for _, n := range msgclients.Names() {
		e, ok := Lookup(n)
		if !ok {
			t.Fatalf("%s not registered", n)
		}
		c, _ := msgclients.Lookup(n)
		if e.Tier != 6 || e.License != c.License || !e.PreferHost || e.EnvVersion != "" {
			t.Errorf("%s: wrong entry %+v", n, e)
		}
		if _, err := e.Resolve(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "no sha256-pinned managed release") {
			t.Errorf("%s: Resolve must refuse auto-provisioning: %v", n, err)
		}
		if !strings.Contains(e.Long, c.BrokerLicense) {
			t.Errorf("%s: help must state the broker licence separately", n)
		}
		if !strings.Contains(e.Long, "bounded") {
			t.Errorf("%s: help must state that subscriptions are bounded", n)
		}
		if cmd := e.NewCmd(); cmd.Name() != n || !cmd.DisableFlagParsing {
			t.Errorf("%s: NewCmd wrong", n)
		}
	}
	if e, _ := Lookup("mosquitto"); !strings.Contains(e.Long, "mosquitto_pub and mosquitto_sub") {
		t.Errorf("mosquitto help must name both host binaries: %s", e.Long)
	}
}
