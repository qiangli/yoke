package engine

import (
	"os"
	"strings"
	"testing"
)

func TestPodmanAssetMatch(t *testing.T) {
	tests := []struct {
		name   string
		asset  string
		goos   string
		goarch string
		want   bool
	}{
		{"windows amd64", "podman-remote-release-windows_amd64.zip", "windows", "amd64", true},
		{"windows arm64", "podman-remote-release-windows_arm64.zip", "windows", "arm64", true},
		{"darwin arm64", "podman-remote-release-darwin_arm64.zip", "darwin", "arm64", true},
		{"linux x86 alias", "podman-remote-static-linux_amd64.tar.gz", "linux", "amd64", true},
		{"reject installer", "podman-installer-windows-amd64.msi", "windows", "amd64", false},
		{"reject wrong os", "podman-remote-release-windows_amd64.zip", "darwin", "amd64", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podmanAssetMatch(tt.asset, tt.goos, tt.goarch); got != tt.want {
				t.Fatalf("podmanAssetMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPrependPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := prependPath([]string{"A=1", "Path=old"}, "new")
	if len(got) != 2 || got[1] != "Path=new"+sep+"old" {
		t.Fatalf("prependPath existing = %#v", got)
	}
	got = prependPath([]string{"A=1"}, "new")
	if !containsEnv(got, "PATH=new") {
		t.Fatalf("prependPath missing PATH = %#v", got)
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if strings.EqualFold(e, want) {
			return true
		}
	}
	return false
}
