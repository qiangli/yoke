package src_test

import (
	"github.com/qiangli/yoke/pkg/src"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpstreamTierCompatibility(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, "legacy.git")
	if err := os.MkdirAll(src.SrcDir(home), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src.SrcDir(home), "upstreams.json"), []byte(`{"yoke":"`+legacy+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	u, err := src.ConfiguredTier(home, "yoke", "upstream")
	if err != nil || u != legacy {
		t.Fatalf("legacy tier: %q %v", u, err)
	}
	if err := src.SetUpstream(home, "yoke", legacy); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(src.SrcDir(home), "upstreams.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"yoke": "`+legacy+`"`) {
		t.Fatalf("legacy form: %s", raw)
	}
	org := filepath.Join(home, "org.git")
	if err := src.SetTier(home, "yoke", "org", org); err != nil {
		t.Fatal(err)
	}
	u, err = src.ConfiguredUpstream(home, "yoke")
	if err != nil || u != legacy {
		t.Fatalf("legacy API: %q %v", u, err)
	}
	u, err = src.ConfiguredTier(home, "yoke", "org")
	if err != nil || u != org {
		t.Fatalf("org tier: %q %v", u, err)
	}
	if err := src.SetUpstream(home, "yoke", org); err != nil {
		t.Fatal(err)
	}
	u, err = src.ConfiguredTier(home, "yoke", "org")
	if err != nil || u != org {
		t.Fatalf("org after legacy setter: %q %v", u, err)
	}
}

func TestCustomAppPromotionDoesNotPermitSourceFetch(t *testing.T) {
	home := t.TempDir()
	for _, tier := range []string{"org", "upstream"} {
		if got, err := src.PromotionTier(home, "hello-world", tier); err != nil || got != "" {
			t.Fatalf("unconfigured app %s = %q, %v", tier, got, err)
		}
		remote := filepath.Join(home, tier+".git")
		if err := src.SetPromotionTier(home, "hello-world", tier, remote); err != nil {
			t.Fatal(err)
		}
		if got, err := src.PromotionTier(home, "hello-world", tier); err != nil || got != remote {
			t.Fatalf("configured app %s = %q, %v", tier, got, err)
		}
	}
	if got, err := src.PromotionUpstream(home, "hello-world"); err != nil || got != filepath.Join(home, "upstream.git") {
		t.Fatalf("explicit upstream = %q, %v", got, err)
	}
	// Promotion configuration must never widen source acquisition. Even an
	// explicitly configured custom app remains refused before fetching.
	if _, err := src.Fetch(home, "hello-world"); err == nil || !strings.Contains(err.Error(), "refusing proprietary or unknown repo") {
		t.Fatalf("source fetch refusal = %v", err)
	}
	if err := src.SetUpstream(home, "hello-world", filepath.Join(home, "other.git")); err == nil {
		t.Fatal("source upstream setter accepted a custom app")
	}
	if _, err := src.ConfiguredTier(home, "hello-world", "org"); err == nil {
		t.Fatal("source tier reader accepted a custom app")
	}
	for _, app := range []string{"", ".", "..", "../escape", `a\b`, "bad\x00name"} {
		if err := src.SetPromotionTier(home, app, "org", "somewhere"); err == nil {
			t.Fatalf("accepted invalid app name %q", app)
		}
	}
}
