package gh

import "testing"

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
