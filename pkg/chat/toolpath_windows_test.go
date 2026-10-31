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

func TestResolveToolBinaryFindsToolOnMSYSPath(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "agentx.exe")
	if err := os.WriteFile(tool, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(dir)
	msys := "/" + strings.ToLower(vol[:1]) + filepath.ToSlash(strings.TrimPrefix(dir, vol))
	t.Setenv("PATH", "/nonexistent:"+msys)
	t.Setenv("PATHEXT", ".EXE;.CMD")
	if got := ResolveToolBinary("agentx"); filepath.Clean(got) != tool {
		t.Fatalf("got %q want %q", got, tool)
	}
}
