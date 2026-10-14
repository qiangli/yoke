package osvscanner

import (
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Name != "osv-scanner" || s.Repo != "google/osv-scanner" || s.Version != DefaultVersion {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Member != "" || s.AssetMatch == nil {
		t.Fatalf("spec member/assetMatch incorrect: %+v", s)
	}
	if Spec("v2.6.0").Version != "v2.6.0" {
		t.Fatal("version override not honored")
	}
}

func TestAssetMatch(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		// 5 supported targets
		{"osv-scanner_darwin_amd64", "darwin", "amd64", true},
		{"osv-scanner_darwin_arm64", "darwin", "arm64", true},
		{"osv-scanner_linux_amd64", "linux", "amd64", true},
		{"osv-scanner_linux_arm64", "linux", "arm64", true},
		{"osv-scanner_windows_amd64.exe", "windows", "amd64", true},

		// negative cases
		{"osv-scanner_darwin_amd64", "linux", "amd64", false},        // wrong os
		{"osv-scanner_darwin_arm64", "darwin", "amd64", false},       // wrong arch
		{"osv-scanner_linux_amd64", "windows", "amd64", false},       // wrong os
		{"osv-scanner_linux_arm64", "linux", "amd64", false},         // wrong arch
		{"osv-scanner_windows_amd64.exe", "darwin", "amd64", false},  // wrong os
		{"osv-scanner_SHA256SUMS", "linux", "amd64", false},          // checksum file
		{"multiple.intoto.jsonl", "linux", "amd64", false},           // provenance attestation
		{"osv-scanner_windows_arm64.exe", "windows", "amd64", false}, // wrong arch
	}

	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %q, %q) = %v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestPins(t *testing.T) {
	targets := []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64"}
	for _, target := range targets {
		sha, ok := binmgr.PinnedSHA256("osv-scanner", DefaultVersion, target)
		if !ok || len(sha) != 64 {
			t.Errorf("osv-scanner pin missing or invalid for %s: ok=%v, sha=%q", target, ok, sha)
		}
	}
}
