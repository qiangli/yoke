package kopia

import "testing"

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Repo != "kopia/kopia" || s.Name != "kopia" || s.Version != "latest" {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Member != "kopia" {
		t.Fatalf("archive member should be the kopia binary, got %q", s.Member)
	}
	if s.AssetMatch == nil {
		t.Fatal("kopia spec needs a custom AssetMatch (macOS/x64 tokens)")
	}
	if Spec("v0.18.0").Version != "v0.18.0" {
		t.Fatal("version override not honored")
	}
}

// Kopia's release tokens differ from Go's: darwin→macOS, amd64→x64.
func TestAssetMatch_MapsTokens(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"kopia-0.18.0-linux-x64.tar.gz", "linux", "amd64", true},    // amd64→x64
		{"kopia-0.18.0-macOS-arm64.tar.gz", "darwin", "arm64", true}, // darwin→macOS
		{"kopia-0.18.0-macOS-x64.tar.gz", "darwin", "amd64", true},
		{"kopia-0.18.0-windows-x64.zip", "windows", "amd64", true},
		{"kopia-0.18.0-linux-arm64.tar.gz", "darwin", "arm64", false}, // wrong os
		{"kopia-0.18.0-linux-x64.tar.gz", "linux", "arm64", false},    // wrong arch
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %s/%s)=%v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}
