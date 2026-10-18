package registry

import (
	"context"
	"testing"

	"github.com/qiangli/yoke/external/gws"
)

func TestGwsEntry(t *testing.T) {
	e, ok := Lookup("gws")
	if !ok {
		t.Fatal("gws not registered")
	}
	if e.Tier != 6 {
		t.Errorf("gws tier = %d, want 6 (cloud)", e.Tier)
	}
	if e.License != gws.License {
		t.Errorf("gws license = %q, want %q (Apache-2.0)", e.License, gws.License)
	}
	if e.EnvVersion != "GWS_VERSION" {
		t.Errorf("gws EnvVersion = %q, want GWS_VERSION", e.EnvVersion)
	}
	if !e.PreferHost {
		t.Error("gws must be host-first (PreferHost) until sha256 pins land in pkg/binmgr/pins.go")
	}
	if e.Synopsis == "" {
		t.Error("gws has no synopsis")
	}
	if e.Resolve == nil {
		t.Error("gws has no Resolve")
	}
	if cmd := e.NewCmd(); cmd.Name() != "gws" || !cmd.DisableFlagParsing {
		t.Errorf("gws NewCmd wrong: name=%q disableFlags=%v", cmd.Name(), cmd.DisableFlagParsing)
	}
}

// The pinned release resolves to the reviewed repo/tag even with no input.
func TestGwsResolvePinned(t *testing.T) {
	e, ok := Lookup("gws")
	if !ok {
		t.Fatal("gws not registered")
	}
	tool, err := e.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve(\"\") failed: %v", err)
	}
	if tool.Name != "gws" {
		t.Errorf("tool name = %q, want gws", tool.Name)
	}
	// The resolved version must be the reviewed pin, never "latest".
	if tool.Version != gws.PinnedVersion {
		t.Errorf("tool version = %q, want pinned %q", tool.Version, gws.PinnedVersion)
	}
}
