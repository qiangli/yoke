// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// The common service registry owns the apps lifecycle. Its seeded exec must
// expand to flags the foreground server accepts, with safe defaults.
//
// The seed is ENABLED: f14961cb ("fleet: enable developer web surfaces by
// default", Sprint 436, Story #2035) turned apps and meet on so a paired
// developer host still has both human-facing front doors after an upgrade —
// seeding either disabled strands sprint and meeting access until someone is
// already on the host to repair it. pkg/fleet's TestDeveloperSurfaceSeedsAreEnabled
// pins that intent; this test asserts the same thing from the console side so
// the pair cannot drift apart again. Safe defaults live in the exec flags
// below (loopback bind, pairing off), not in refusing to start.
func TestAppsUnitExecMatchesServeFlags(t *testing.T) {
	u, ok := fleet.New().Service("apps")
	if !ok {
		t.Fatal("apps service seed is missing")
	}
	if !u.Enabled || u.ProviderName() != fleet.ServiceProviderProcess {
		t.Fatalf("seed = enabled %v provider %s, want enabled process", u.Enabled, u.ProviderName())
	}
	argv, err := u.ExpandExec("")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(argv, " "), "bashy apps serve --port 22749 --bind 127.0.0.1 --pair=false --launcher="; got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	serve := newServeCmd()
	if err := serve.ParseFlags(argv[3:]); err != nil {
		t.Fatalf("serve rejects the unit's argv: %v", err)
	}
	if p, _ := serve.Flags().GetBool("pair"); p {
		t.Fatal("seed arms pairing")
	}
	if a := u.Expose.App; a == nil || !a.RequireLogin || !a.Elevation || !a.TrustCloudIdentity {
		t.Fatalf("expose.app = %+v, want login+elevation+cloud identity", a)
	}
	h := do(newTestHandler(t, Options{}), "GET", "/healthz", "127.0.0.1:5555", nil)
	if !strings.Contains(h.Body.String(), `"service":"`+otelServiceName+`"`) {
		t.Fatalf("/healthz = %s, want service %q", h.Body.String(), otelServiceName)
	}
}

func TestAppsHasNoLegacyServiceFacade(t *testing.T) {
	cmd := NewAppsCmd()
	cmd.SetArgs([]string{"service"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("legacy app service command is still registered")
	}
}
