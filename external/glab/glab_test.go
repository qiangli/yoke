package glab

import (
	"context"
	"strings"
	"testing"
)

func TestAdmissionConsts(t *testing.T) {
	if License != "MIT" {
		t.Errorf("License = %q, want MIT", License)
	}
	if Source != "gitlab-org/cli" {
		t.Errorf("Source = %q, want gitlab-org/cli", Source)
	}
	if SourceURL != "https://gitlab.com/gitlab-org/cli" {
		t.Errorf("SourceURL = %q", SourceURL)
	}
	if HostDefault != "https://gitlab.com" {
		t.Errorf("HostDefault = %q", HostDefault)
	}
}

func TestInstallHintNonEmptyAndVendorPath(t *testing.T) {
	h := InstallHint()
	if strings.TrimSpace(h) == "" {
		t.Fatal("InstallHint returned empty")
	}
	if !strings.Contains(h, "gitlab-org/cli") {
		t.Errorf("InstallHint should point at the vendor project, got %q", h)
	}
}

// Resolution must never turn any version into provisioning, nor echo
// potentially secret caller input in the actionable diagnostic.
func TestResolveRefusesEveryVersion(t *testing.T) {
	for _, version := range []string{"", "latest", "v1.48.0", "s457-canary-secret"} {
		tool, err := Resolve(context.Background(), version)
		if err == nil || tool.Name != "" || len(tool.Assets) != 0 {
			t.Fatalf("version %q admitted a download", version)
		}
		if !strings.Contains(err.Error(), "not found on PATH") || !strings.Contains(err.Error(), "gitlab-org/cli") {
			t.Fatalf("not actionable: %v", err)
		}
		if strings.Contains(err.Error(), "s457-canary-secret") {
			t.Fatal("diagnostic disclosed canary")
		}
	}
}

func TestPathMissingIsActionable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Path(); err == nil {
		t.Fatal("Path succeeded with no glab on PATH")
	} else if !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("not actionable: %v", err)
	}
}

func TestNewGlabCmd(t *testing.T) {
	cmd := NewGlabCmd()
	if cmd.Name() != "glab" {
		t.Errorf("cmd name = %q, want glab", cmd.Name())
	}
	if !cmd.DisableFlagParsing {
		t.Error("glab cmd must pass args through (DisableFlagParsing)")
	}
}

func TestChildEnvDisablesTracking(t *testing.T) {
	out := ChildEnv([]string{"FOO=1"})
	found := false
	for _, e := range out {
		if len(e) >= 4 && e[:4] == "FOO=" {
			found = true
		}
	}
	if !found {
		t.Error("ChildEnv dropped the parent env")
	}
}

// Per-client runtime: short-lived client only, never a resident mode.
func TestClientRuntimeIsShortLived(t *testing.T) {
	r := ClientRuntime()
	if r.Mode != "short-lived-client" {
		t.Errorf("Mode = %q, want short-lived-client", r.Mode)
	}
	if r.LongLived {
		t.Error("glab must never run long-lived here")
	}
	if strings.TrimSpace(r.Note) == "" {
		t.Error("runtime note is empty")
	}
}
