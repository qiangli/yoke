package gcloud_test

import (
	"context"
	"github.com/qiangli/yoke/external/registry"
	"os"
	"path/filepath"
	"testing"
)

// Opt-in admission probe: the registry owner must fix this before host-only admission.
func TestQualificationProbeCacheMustNotReplaceHostSDK(t *testing.T) {
	if os.Getenv("S457_AUDIT_HOST_ONLY") != "1" {
		t.Skip("known registry cache fallback; set S457_AUDIT_HOST_ONLY=1 to reproduce admission failure")
	}
	t.Setenv("PATH", t.TempDir())
	cache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cache)
	if err := os.WriteFile(filepath.Join(cache, "gcloud"), []byte("not an admitted SDK"), 0700); err != nil {
		t.Fatal(err)
	}
	entry, _ := registry.Lookup("gcloud")
	got, err := entry.Ensure(context.Background())
	if err == nil {
		t.Fatalf("HOST-ONLY GAP: missing PATH SDK resolved unverified cache fixture (%s); no executable was run", filepath.Base(got))
	}
}
