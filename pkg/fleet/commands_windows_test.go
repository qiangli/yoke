//go:build windows

package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutableMSYSDrivePathPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.exe"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if len(dir) < 3 || dir[1] != ':' {
		t.Skipf("temp dir %q is not a drive path", dir)
	}
	msys := "/" + strings.ToLower(dir[:1]) + filepath.ToSlash(dir[2:]) + "/tool"
	c := Command{Name: "tool", Exec: []string{msys}}
	if _, ok := c.Executable(); !ok {
		t.Fatalf("Executable(%q) not present, want present", msys)
	}
}
