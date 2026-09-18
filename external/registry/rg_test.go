package registry

import "testing"

func TestRgRegistered(t *testing.T) {
	e, ok := Lookup("rg")
	if !ok {
		t.Fatal("rg not registered")
	}
	if e.Tier != 2 {
		t.Errorf("rg tier = %d, want 2 (userland)", e.Tier)
	}
	if e.License != "MIT OR Unlicense" {
		t.Errorf("rg license = %q, want MIT OR Unlicense", e.License)
	}
	if e.EnvVersion != "RG_VERSION" {
		t.Errorf("rg EnvVersion = %q, want RG_VERSION", e.EnvVersion)
	}
	if e.Resolve == nil {
		t.Error("rg has no Resolve")
	}
}

func TestRgAssetMatch(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		goarch string
		want   bool
	}{
		{"ripgrep-15.1.0-x86_64-pc-windows-msvc.zip", "windows", "amd64", true},
		{"ripgrep-15.1.0-aarch64-pc-windows-msvc.zip", "windows", "arm64", true},
		{"ripgrep-15.1.0-x86_64-apple-darwin.tar.gz", "darwin", "amd64", true},
		{"ripgrep-15.1.0-aarch64-apple-darwin.tar.gz", "darwin", "arm64", true},
		{"ripgrep-15.1.0-x86_64-unknown-linux-musl.tar.gz", "linux", "amd64", true},
		{"ripgrep-15.1.0-aarch64-unknown-linux-gnu.tar.gz", "linux", "arm64", true},
		{"ripgrep-15.1.0-x86_64-pc-windows-msvc.zip.sha256", "windows", "amd64", true},
		{"ripgrep-15.1.0-aarch64-pc-windows-msvc.zip", "windows", "amd64", false},
		{"ripgrep-15.1.0-x86_64-pc-windows-gnu.zip", "windows", "amd64", false},
		{"not-ripgrep-15.1.0-x86_64-pc-windows-msvc.zip", "windows", "amd64", false},
	}
	for _, tt := range tests {
		if got := rgAssetMatch(tt.name, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("rgAssetMatch(%q, %q, %q) = %v, want %v", tt.name, tt.goos, tt.goarch, got, tt.want)
		}
	}
}
