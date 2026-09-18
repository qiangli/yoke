package act

import "testing"

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Name != "act" || s.Repo != "nektos/act" || s.Version != DefaultVersion {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Member != "act" || s.AssetMatch == nil {
		t.Fatalf("spec missing member/assetMatch: %+v", s)
	}
	if Spec("v0.2.80").Version != "v0.2.80" {
		t.Fatal("version override not honored")
	}
}

func TestAssetMatch(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"act_Linux_x86_64.tar.gz", "linux", "amd64", true},
		{"act_Linux_arm64.tar.gz", "linux", "arm64", true},
		{"act_Darwin_arm64.tar.gz", "darwin", "arm64", true},
		{"act_Windows_x86_64.zip", "windows", "amd64", true},
		{"act_Linux_x86_64.tar.gz", "darwin", "amd64", false}, // wrong os
		{"act_Linux_arm64.tar.gz", "linux", "amd64", false},   // wrong arch
		{"checksums.txt", "linux", "amd64", false},
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q,%s,%s)=%v want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}
