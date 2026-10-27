package spacetime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLookPathMSYSDrivePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("MSYS drive paths are Windows-only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.exe"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(dir)
	rest := filepath.ToSlash(strings.TrimPrefix(dir, vol))
	msys := "/" + strings.ToLower(vol[:1]) + rest + "/tool"
	if _, err := LookPath(msys); err != nil {
		t.Fatalf("LookPath(%q): %v", msys, err)
	}
}
