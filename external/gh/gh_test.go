package gh

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Name != "gh" || s.Repo != "cli/cli" || s.Version != DefaultVersion {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Member != "gh" || s.AssetMatch == nil {
		t.Fatalf("spec missing member/assetMatch: %+v", s)
	}
	if Spec("v2.62.0").Version != "v2.62.0" {
		t.Fatal("version override not honored")
	}
}

func TestAssetMatch(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"gh_2.62.0_linux_amd64.tar.gz", "linux", "amd64", true},
		{"gh_2.62.0_linux_arm64.tar.gz", "linux", "arm", false},
		{"gh_2.62.0_linux_amd64.tar.gz.sha256", "linux", "amd64", false},
		{"gh_2.62.0_linux_amd64.deb", "linux", "amd64", false},
		{"other_linux_amd64.zip", "linux", "amd64", false},
		{"gh_2.62.0_linux_arm64.tar.gz", "linux", "arm64", true},
		{"gh_2.62.0_macOS_arm64.zip", "darwin", "arm64", true},
		{"gh_2.62.0_windows_amd64.zip", "windows", "amd64", true},
		{"gh_2.62.0_linux_amd64.tar.gz", "windows", "amd64", false}, // wrong os
		{"gh_2.62.0_linux_arm64.tar.gz", "linux", "amd64", false},   // wrong arch
		{"gh_2.62.0_macOS_amd64.zip", "linux", "amd64", false},      // wrong os
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q,%s,%s)=%v want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

// The default is a reviewed release with a committed digest on every shipped
// platform; "latest" and unpinned versions are refused before any network.
func TestDefaultVersionPinnedAndUnpinnedRefused(t *testing.T) {
	if DefaultVersion == "latest" || DefaultVersion == "" {
		t.Fatalf("DefaultVersion %q is not a reviewed pin", DefaultVersion)
	}
	for _, p := range []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"} {
		if sha, ok := binmgr.PinnedSHA256("gh", DefaultVersion, p); !ok || len(sha) != 64 {
			t.Errorf("gh %s has no committed pin for %s", DefaultVersion, p)
		}
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	for _, v := range []string{"latest", "v0.0.1-unreviewed"} {
		if _, err := Ensure(context.Background(), v); err == nil || !strings.Contains(err.Error(), "no committed sha256 pin") {
			t.Errorf("Ensure(%q) = %v, want fail-closed pin refusal", v, err)
		}
	}
}
