package llmbudget

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultFromEnvMSYSStatePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("MSYS drive paths are Windows-only")
	}
	dir := t.TempDir()
	vol := filepath.VolumeName(dir)
	rest := filepath.ToSlash(strings.TrimPrefix(dir, vol))
	t.Setenv("BASHY_LLM_BUDGET_STATE", "/"+strings.ToLower(vol[:1])+rest+"/meter.json")
	got := DefaultFromEnv().cfg.StatePath
	if !strings.EqualFold(filepath.Clean(got), filepath.Join(dir, "meter.json")) {
		t.Fatalf("StatePath = %q, want native under %q", got, dir)
	}
	_ = os.Remove(got)
}

func TestNativeStatePathNonWindowsUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	if got := nativeStatePath("/tmp/x.json"); got != "/tmp/x.json" {
		t.Fatalf("got %q", got)
	}
}
