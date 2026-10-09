// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/loom"
	"github.com/qiangli/yoke/pkg/atlas"
)

func withOutpostAdminResolver(t *testing.T, cfg outpostConfig, cfgErr error, defaultAddr string) {
	t.Helper()
	prevShow := outpostConfigShow
	prevDefault := outpostAdminDefaultAddr
	outpostConfigShow = func() (outpostConfig, error) { return cfg, cfgErr }
	outpostAdminDefaultAddr = defaultAddr
	resetOutpostAdminAddrCache()
	t.Cleanup(func() {
		outpostConfigShow = prevShow
		outpostAdminDefaultAddr = prevDefault
		resetOutpostAdminAddrCache()
	})
}

func builtinPanelByName(name string) (Panel, bool) {
	for _, p := range builtinPanels() {
		if p.Name == name {
			return p, true
		}
	}
	return Panel{}, false
}

func TestBuiltinPanelsIncludeLoomAndHost(t *testing.T) {
	t.Setenv("OUTPOST_ADMIN_ADDR", "")
	withOutpostAdminResolver(t, outpostConfig{}, nil, "127.0.0.1:17777")

	for _, name := range []string{"loom", "host"} {
		p, ok := builtinPanelByName(name)
		if !ok {
			t.Fatalf("builtinPanels() missing %q", name)
		}
		if p.Source != "builtin" {
			t.Errorf("%s Source = %q, want builtin", name, p.Source)
		}
		if p.Mode != atlas.WebProxy {
			t.Errorf("%s Mode = %q, want proxy", name, p.Mode)
		}
	}
}

func TestLoomBuiltinCarriesTheApprovedIcon(t *testing.T) {
	p, ok := builtinPanelByName("loom")
	if !ok {
		t.Fatal("builtinPanels() missing loom")
	}
	if p.Icon != "M4 5h16M4 19h16M9 5v14M15 5v14M4 12h16" {
		t.Errorf("loom icon = %q", p.Icon)
	}
	if p.Port != loom.DefaultProxyPort {
		t.Errorf("loom port = %d, want loom.DefaultProxyPort %d", p.Port, loom.DefaultProxyPort)
	}
}

func TestHostTileResolvesTheAdminAddress(t *testing.T) {
	t.Run("environment wins", func(t *testing.T) {
		t.Setenv("OUTPOST_ADMIN_ADDR", "127.0.0.2:19999")
		withOutpostAdminResolver(t, outpostConfig{AdminAddr: "127.0.0.3:18888"}, nil, "127.0.0.1:17777")
		p := hostBuiltinPanel()
		if p.ProxyHost != "127.0.0.2" || p.Port != 19999 || !p.Available {
			t.Fatalf("host panel = %+v", p)
		}
	})

	t.Run("configured value next", func(t *testing.T) {
		t.Setenv("OUTPOST_ADMIN_ADDR", "")
		withOutpostAdminResolver(t, outpostConfig{AdminAddr: "http://127.0.0.3:18888"}, nil, "127.0.0.1:17777")
		p := hostBuiltinPanel()
		if p.ProxyHost != "127.0.0.3" || p.Port != 18888 || !p.Available {
			t.Fatalf("host panel = %+v", p)
		}
	})

	t.Run("default last", func(t *testing.T) {
		t.Setenv("OUTPOST_ADMIN_ADDR", "")
		withOutpostAdminResolver(t, outpostConfig{}, nil, "127.0.0.1:17777")
		p := hostBuiltinPanel()
		if p.ProxyHost != "127.0.0.1" || p.Port != 17777 || !p.Available {
			t.Fatalf("host panel = %+v", p)
		}
	})
}

func TestHostTileDegradesWhenNoAdminAddress(t *testing.T) {
	t.Setenv("OUTPOST_ADMIN_ADDR", "")
	withOutpostAdminResolver(t, outpostConfig{}, nil, "")

	p := hostBuiltinPanel()
	if p.Available || p.Note == "" {
		t.Fatalf("host panel = %+v, want unavailable with a note", p)
	}
	st := (&probeCache{}).Probe(context.Background(), []Panel{p})
	if len(st) != 1 || st[0].Status != StatusUnavailable || !strings.Contains(st[0].Note, "admin address") {
		t.Fatalf("status = %+v, want unavailable host tile with note", st)
	}
}

func TestRegisteredAppCannotShadowTheBuiltinLoom(t *testing.T) {
	writeAppRecord(t, "loom", "name: loom\nlabel: Registered Loom\nport: 9000\n")
	for _, p := range Discover() {
		if p.Name == "loom" && p.Source != "builtin" {
			t.Fatalf("loom shadowed by %+v", p)
		}
	}
	if err := ValidateRegisteredApp(appRecord("loom", 9000)); err == nil {
		t.Fatal("registered app validation accepted builtin loom")
	}
}

func TestHostEnvironmentOverrideDoesNotReadConfig(t *testing.T) {
	t.Setenv("OUTPOST_ADMIN_ADDR", "127.0.0.1:19999")
	withOutpostAdminResolver(t, outpostConfig{}, nil, "127.0.0.1:17777")
	outpostConfigShow = func() (outpostConfig, error) {
		t.Error("explicit admin address must not launch outpost config show")
		return outpostConfig{}, nil
	}
	p := hostBuiltinPanel()
	if p.Port != 19999 || !p.Available {
		t.Fatalf("host panel = %+v", p)
	}
}
