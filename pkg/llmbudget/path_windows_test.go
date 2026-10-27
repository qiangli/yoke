//go:build windows

package llmbudget

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewConvertsMSYSStatePath(t *testing.T) {
	dir := t.TempDir()
	rel, err := filepath.Rel(os.TempDir(), dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("temp dir %q is not below Windows temp %q: rel=%q err=%v", dir, os.TempDir(), rel, err)
	}
	native := filepath.Join(dir, "meter.json")
	msys := "/tmp/" + filepath.ToSlash(filepath.Join(rel, "meter.json"))

	g := New(Config{
		StatePath: msys,
		Models: map[string]Model{
			"fixture": {Name: "fixture", Kind: "local", CostMicro: 1},
		},
	})
	g.Record("fixture", 1, 1, 0)
	if _, err := os.Stat(native); err != nil {
		t.Fatalf("meter was not written at native path %q: %v", native, err)
	}
}
