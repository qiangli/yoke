package gcloud_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/registry"
)

// Regression for the Sprint 457 host-only gap: with no SDK on PATH, the
// registry used to return an unverified cached gcloud. It must refuse with
// the actionable missing-SDK diagnostic instead. No fixture is executed.
func TestQualificationProbeCacheMustNotReplaceHostSDK(t *testing.T) {
	name := "gcloud"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	// Both cache layouts CachedBinary knows: latest-wins and version-pinned.
	for _, rel := range []string{name, filepath.Join("gcloud", "575.0.0", name)} {
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			cache := t.TempDir()
			t.Setenv("BASHY_BIN_CACHE", cache)
			p := filepath.Join(cache, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("not an admitted SDK"), 0o700); err != nil {
				t.Fatal(err)
			}
			entry, _ := registry.Lookup("gcloud")
			got, err := entry.Ensure(context.Background())
			if err == nil {
				t.Fatalf("HOST-ONLY GAP: missing PATH SDK resolved unverified cache fixture (%s)", filepath.Base(got))
			}
			if got != "" || !strings.Contains(err.Error(), "not found on PATH") || !strings.Contains(err.Error(), "cloud.google.com/sdk/docs/install") {
				t.Fatalf("missing SDK diagnostic not actionable: %q %v", got, err)
			}
		})
	}
}
