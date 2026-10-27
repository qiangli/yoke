//go:build windows

package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveToolBinaryConvertsMSYSDrivePath(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool.exe")
	if err := os.WriteFile(tool, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	vol := filepath.VolumeName(tool)
	rest := filepath.ToSlash(strings.TrimPrefix(tool, vol))
	msys := "/" + strings.ToLower(vol[:1]) + rest
	if got := ResolveToolBinary(msys); filepath.Clean(got) != tool {
		t.Fatalf("ResolveToolBinary(%q) = %q, want native program path %q", msys, got, tool)
	}
}
