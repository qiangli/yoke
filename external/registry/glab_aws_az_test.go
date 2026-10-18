package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/aws"
	"github.com/qiangli/yoke/external/az"
	"github.com/qiangli/yoke/external/glab"
	"github.com/qiangli/yoke/pkg/atlas"
)

// Sprint 457 story bedc320542eb: glab, aws and az register as tier-6
// host-first cloud CLIs. Each passes its own acceptance; partial
// completion cannot close the group.
func TestGlabEntry(t *testing.T) {
	e, ok := Lookup("glab")
	if !ok {
		t.Fatal("glab not registered")
	}
	if e.Tier != 6 {
		t.Errorf("glab tier = %d, want 6 (cloud)", e.Tier)
	}
	if e.License != glab.License {
		t.Errorf("glab license = %q, want %q", e.License, glab.License)
	}
	if e.License != "MIT" {
		t.Errorf("glab license = %q, want MIT", e.License)
	}
	if !e.PreferHost {
		t.Error("glab must be host-first (PreferHost): no pin, no download")
	}
	if e.EnvVersion != "" || e.DefaultVersion != "" {
		t.Errorf("glab must not pin versions (EnvVersion=%q DefaultVersion=%q)", e.EnvVersion, e.DefaultVersion)
	}
	if e.Synopsis == "" || e.Resolve == nil {
		t.Error("glab has no synopsis/Resolve")
	}
	if cmd := e.NewCmd(); cmd.Name() != "glab" || !cmd.DisableFlagParsing {
		t.Errorf("glab NewCmd wrong: name=%q disableFlags=%v", cmd.Name(), cmd.DisableFlagParsing)
	}
	if d, ok := atlas.ExternalPlatforms("glab"); !ok || len(d.OS) == 0 {
		t.Error("glab has no atlas platform declaration")
	}
}

func TestAwsEntry(t *testing.T) {
	e, ok := Lookup("aws")
	if !ok {
		t.Fatal("aws not registered")
	}
	if e.Tier != 6 {
		t.Errorf("aws tier = %d, want 6 (cloud)", e.Tier)
	}
	if e.License != aws.License {
		t.Errorf("aws license = %q, want %q", e.License, aws.License)
	}
	if e.License != "Apache-2.0" {
		t.Errorf("aws license = %q, want Apache-2.0", e.License)
	}
	if !e.PreferHost {
		t.Error("aws must be host-first (PreferHost): no pin, no download")
	}
	if e.EnvVersion != "" || e.DefaultVersion != "" {
		t.Errorf("aws must not pin versions (EnvVersion=%q DefaultVersion=%q)", e.EnvVersion, e.DefaultVersion)
	}
	if e.Synopsis == "" || e.Resolve == nil {
		t.Error("aws has no synopsis/Resolve")
	}
	if cmd := e.NewCmd(); cmd.Name() != "aws" || !cmd.DisableFlagParsing {
		t.Errorf("aws NewCmd wrong: name=%q disableFlags=%v", cmd.Name(), cmd.DisableFlagParsing)
	}
	if d, ok := atlas.ExternalPlatforms("aws"); !ok || len(d.OS) == 0 {
		t.Error("aws has no atlas platform declaration")
	}
}

func TestAzEntry(t *testing.T) {
	e, ok := Lookup("az")
	if !ok {
		t.Fatal("az not registered")
	}
	if e.Tier != 6 {
		t.Errorf("az tier = %d, want 6 (cloud)", e.Tier)
	}
	if e.License != az.License {
		t.Errorf("az license = %q, want %q", e.License, az.License)
	}
	if e.License != "MIT" {
		t.Errorf("az license = %q, want MIT", e.License)
	}
	if !e.PreferHost {
		t.Error("az must be host-first (PreferHost): no pin, no download")
	}
	if e.EnvVersion != "" || e.DefaultVersion != "" {
		t.Errorf("az must not pin versions (EnvVersion=%q DefaultVersion=%q)", e.EnvVersion, e.DefaultVersion)
	}
	if e.Synopsis == "" || e.Resolve == nil {
		t.Error("az has no synopsis/Resolve")
	}
	if cmd := e.NewCmd(); cmd.Name() != "az" || !cmd.DisableFlagParsing {
		t.Errorf("az NewCmd wrong: name=%q disableFlags=%v", cmd.Name(), cmd.DisableFlagParsing)
	}
	if d, ok := atlas.ExternalPlatforms("az"); !ok || len(d.OS) == 0 {
		t.Error("az has no atlas platform declaration")
	}
}

// The host-first entries must refuse provisioning through the registry
// too — with no host binary and no version pinned, Ensure goes to Resolve
// and its actionable diagnostic, never an unverified binary.
func TestHostFirstEntriesRefuseWithoutHost(t *testing.T) {
	for _, name := range []string{"glab", "aws", "az"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			t.Setenv("BASHY_BIN_CACHE", t.TempDir())
			e, ok := Lookup(name)
			if !ok {
				t.Fatalf("%s not registered", name)
			}
			if _, err := e.Ensure(context.Background()); err == nil {
				t.Fatalf("%s Ensure provisioned without a host binary", name)
			} else if !strings.Contains(err.Error(), "not found on PATH") {
				t.Fatalf("%s Ensure error not actionable: %v", name, err)
			}
		})
	}
}
