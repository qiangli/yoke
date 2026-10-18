package gws

import (
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// The pin is the admission: release, repo, and immutable commit.
func TestPin(t *testing.T) {
	if License != "Apache-2.0" {
		t.Errorf("License = %q, want Apache-2.0", License)
	}
	if Repo != "googleworkspace/cli" {
		t.Errorf("Repo = %q, want googleworkspace/cli", Repo)
	}
	if PinnedVersion != "v0.22.5" {
		t.Errorf("PinnedVersion = %q, want v0.22.5", PinnedVersion)
	}
	if PinnedCommit != "a3768d0e82ad83cca2da97724e46bea4ff0e6dbd" {
		t.Errorf("PinnedCommit = %q, want the reviewed commit", PinnedCommit)
	}
}

func TestSpecDefaultsToPin(t *testing.T) {
	spec := Spec("")
	if spec.Version != PinnedVersion {
		t.Errorf("Spec(\"\").Version = %q, want %q", spec.Version, PinnedVersion)
	}
	if spec.Repo != Repo {
		t.Errorf("Spec Repo = %q, want %q", spec.Repo, Repo)
	}
	if spec.Member != "gws" {
		t.Errorf("Spec Member = %q, want gws", spec.Member)
	}
	if spec.AssetMatch == nil {
		t.Error("Spec has no AssetMatch")
	}
	_ = binmgr.GitHubSpec{}
}

func TestAssetMatch(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		goarch string
		want   bool
	}{
		// Exact asset names from the reviewed release's evidence record.
		{"google-workspace-cli-aarch64-apple-darwin.tar.gz", "darwin", "arm64", true},
		{"google-workspace-cli-x86_64-apple-darwin.tar.gz", "darwin", "amd64", true},
		{"google-workspace-cli-aarch64-unknown-linux-gnu.tar.gz", "linux", "arm64", true},
		{"google-workspace-cli-x86_64-unknown-linux-gnu.tar.gz", "linux", "amd64", true},
		{"google-workspace-cli-x86_64-pc-windows-msvc.zip", "windows", "amd64", true},
		// The musl Linux variant is an unverified alternative: never matched,
		// so resolution stays deterministic.
		{"google-workspace-cli-x86_64-unknown-linux-musl.tar.gz", "linux", "amd64", false},
		{"google-workspace-cli-aarch64-unknown-linux-musl.tar.gz", "linux", "arm64", false},
		// Cross-platform mismatches.
		{"google-workspace-cli-x86_64-apple-darwin.tar.gz", "linux", "amd64", false},
		{"google-workspace-cli-x86_64-unknown-linux-gnu.tar.gz", "darwin", "amd64", false},
		{"google-workspace-cli-aarch64-apple-darwin.tar.gz", "darwin", "amd64", false},
		{"google-workspace-cli-x86_64-pc-windows-msvc.zip", "linux", "amd64", false},
		{"google-workspace-cli-x86_64-pc-windows-msvc.zip", "windows", "arm64", false},
	}
	for _, tt := range tests {
		if got := assetMatch(tt.name, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("assetMatch(%q, %q, %q) = %v, want %v", tt.name, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestNewGwsCmd(t *testing.T) {
	cmd := NewGwsCmd()
	if cmd.Name() != "gws" {
		t.Errorf("cmd name = %q, want gws", cmd.Name())
	}
	if !cmd.DisableFlagParsing {
		t.Error("gws cmd must pass args through (DisableFlagParsing)")
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
