package seaweedfs

import "testing"

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Repo != "seaweedfs/seaweedfs" || s.Name != "seaweedfs" || s.Version != "latest" {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Member != "weed" {
		t.Fatalf("archive member should be the weed binary, got %q", s.Member)
	}
	if s.AssetMatch == nil {
		t.Fatal("seaweedfs spec needs a custom AssetMatch (standard build, not full/large_disk)")
	}
	if Spec("3.80").Version != "3.80" {
		t.Fatal("version override not honored")
	}
}

func TestAssetMatch_PicksStandardArchive(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"linux_amd64.tar.gz", "linux", "amd64", true},
		{"linux_amd64_full.tar.gz", "linux", "amd64", false},       // _full excluded
		{"linux_amd64_large_disk.tar.gz", "linux", "amd64", false}, // _large_disk excluded
		{"darwin_arm64.tar.gz", "darwin", "arm64", true},
		{"linux_arm64.tar.gz", "linux", "amd64", false}, // wrong arch
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %s/%s)=%v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}
