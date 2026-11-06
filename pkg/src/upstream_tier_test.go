package src_test

import (
	"github.com/qiangli/yoke/pkg/src"
	"os"
	"path/filepath"
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
