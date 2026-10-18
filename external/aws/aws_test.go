package aws

import (
	"context"
	"strings"
	"testing"
)

func TestAdmissionConsts(t *testing.T) {
	if License != "Apache-2.0" {
		t.Errorf("License = %q, want Apache-2.0", License)
	}
	if Source != "Amazon Web Services CLI v2" {
		t.Errorf("Source = %q, want Amazon Web Services CLI v2", Source)
	}
	if SourceURL != "https://aws.amazon.com/cli/" {
		t.Errorf("SourceURL = %q", SourceURL)
	}
}

func TestInstallHintNonEmptyAndVendorPath(t *testing.T) {
	h := InstallHint()
	if strings.TrimSpace(h) == "" {
		t.Fatal("InstallHint returned empty")
	}
	if !strings.Contains(h, "aws.amazon.com/cli") {
		t.Errorf("InstallHint should point at the vendor installer, got %q", h)
	}
}

// Resolution must never turn any version into provisioning, nor echo
// potentially secret caller input in the actionable diagnostic.
func TestResolveRefusesEveryVersion(t *testing.T) {
	for _, version := range []string{"", "latest", "2.15.0", "s457-canary-secret"} {
		tool, err := Resolve(context.Background(), version)
		if err == nil || tool.Name != "" || len(tool.Assets) != 0 {
			t.Fatalf("version %q admitted a download", version)
		}
		if !strings.Contains(err.Error(), "not found on PATH") || !strings.Contains(err.Error(), "aws.amazon.com/cli") {
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
		t.Fatal("Path succeeded with no aws on PATH")
	} else if !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("not actionable: %v", err)
	}
}

func TestNewAwsCmd(t *testing.T) {
	cmd := NewAwsCmd()
	if cmd.Name() != "aws" {
		t.Errorf("cmd name = %q, want aws", cmd.Name())
	}
	if !cmd.DisableFlagParsing {
		t.Error("aws cmd must pass args through (DisableFlagParsing)")
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
		t.Error("aws must never run long-lived here")
	}
	if strings.TrimSpace(r.Note) == "" {
		t.Error("runtime note is empty")
	}
}
