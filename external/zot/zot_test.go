package zot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureConfig_SeedsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ensureConfig(dir, "127.0.0.1", 5000)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if cfg != filepath.Join(dir, "config.json") {
		t.Fatalf("cfg path = %s", cfg)
	}
	b, _ := os.ReadFile(cfg)
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	http := parsed["http"].(map[string]any)
	if http["address"] != "127.0.0.1" || http["port"] != "5000" {
		t.Fatalf("http config = %v", http)
	}
	if _, ok := parsed["storage"]; !ok {
		t.Fatal("config missing storage")
	}
	// idempotent: second call must not overwrite
	first := string(b)
	if _, err := ensureConfig(dir, "127.0.0.1", 5000); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(cfg); string(b2) != first {
		t.Fatal("ensureConfig overwrote an existing config")
	}
}

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Repo != "project-zot/zot" || s.Name != "zot" || s.Version != "latest" {
		t.Fatalf("default spec = %+v", s)
	}
	if Spec("v2.1.0").Version != "v2.1.0" {
		t.Fatal("version override not honored")
	}
	if s.AssetMatch == nil {
		t.Fatal("zot spec needs a custom AssetMatch (full build, not minimal)")
	}
}

// The asset names below are the REAL ones from a zot release (verified against
// project-zot/zot v2.1.18). The previous version of this test asserted on
// invented names — "zot-minimal-linux-amd64" and "zot-exporter-linux-amd64",
// neither of which zot publishes — which is exactly why the bug shipped: the
// test passed while the resolver cached `zb`, the load-test benchmark tool.
func TestAssetMatch_PicksFullBuild(t *testing.T) {
	cases := []struct {
		goos, goarch, name string
		want               bool
	}{
		// the full build — the only acceptable match
		{"linux", "amd64", "zot-linux-amd64", true},
		{"darwin", "arm64", "zot-darwin-arm64", true},

		// siblings that ALL carry the os/arch tokens and must NOT match
		{"darwin", "arm64", "zb-darwin-arm64", false},  // benchmark — the shipped bug
		{"darwin", "arm64", "zli-darwin-arm64", false}, // CLI
		{"darwin", "arm64", "zxp-darwin-arm64", false}, // exporter (named zxp, not "exporter")
		{"darwin", "arm64", "zot-darwin-arm64-debug", false},
		{"darwin", "arm64", "zot-darwin-arm64-minimal", false},
		{"linux", "amd64", "zb-linux-amd64", false},
		{"linux", "amd64", "zli-linux-amd64", false},
		{"linux", "amd64", "zxp-linux-amd64", false},
		{"linux", "amd64", "zot-linux-amd64-minimal", false},

		// wrong platform never matches
		{"linux", "amd64", "zot-darwin-arm64", false},
		{"darwin", "arm64", "zot-linux-amd64", false},
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %s/%s) = %v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}

// Exactly one asset from a real release listing may match a given platform —
// otherwise resolution is order-dependent and can silently cache the wrong tool.
func TestAssetMatch_ExactlyOneWinnerPerPlatform(t *testing.T) {
	release := []string{
		"zb-darwin-arm64", "zli-darwin-arm64", "zot-darwin-arm64",
		"zot-darwin-arm64-debug", "zot-darwin-arm64-minimal", "zxp-darwin-arm64",
		"zb-linux-amd64", "zli-linux-amd64", "zot-linux-amd64",
		"zot-linux-amd64-debug", "zot-linux-amd64-minimal", "zxp-linux-amd64",
	}
	for _, p := range []struct{ goos, goarch string }{{"darwin", "arm64"}, {"linux", "amd64"}} {
		var hits []string
		for _, a := range release {
			if assetMatch(a, p.goos, p.goarch) {
				hits = append(hits, a)
			}
		}
		if len(hits) != 1 {
			t.Errorf("%s/%s matched %d assets %v, want exactly 1", p.goos, p.goarch, len(hits), hits)
		}
	}
}
