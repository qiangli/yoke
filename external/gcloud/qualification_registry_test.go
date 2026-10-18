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

func TestQualificationHostSDKAndMissingDiagnostic(t *testing.T) {
	entry, ok := registry.Lookup("gcloud")
	if !ok || !entry.PreferHost {
		t.Fatal("host SDK preference missing")
	}
	host, cache := t.TempDir(), t.TempDir()
	t.Setenv("PATH", host)
	t.Setenv("BASHY_BIN_CACHE", cache)
	name := "gcloud"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	want := filepath.Join(host, name)
	// Never execute this fixture; discovery must return the host path.
	if err := os.WriteFile(want, []byte("fixture, not an SDK"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := entry.Ensure(context.Background())
	if err != nil || got != want {
		t.Fatalf("host discovery: %q %v", got, err)
	}
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	got, err = entry.Ensure(context.Background())
	if got != "" || err == nil || !strings.Contains(err.Error(), "cloud.google.com/sdk/docs/install") {
		t.Fatalf("missing SDK: %q %v", got, err)
	}
}
