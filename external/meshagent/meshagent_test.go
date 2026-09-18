package meshagent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit check is unix-shaped")
	}
	// A non-executable $OUTPOST_BIN resolves false.
	t.Setenv("OUTPOST_BIN", filepath.Join(t.TempDir(), "nope"))
	if _, ok := Resolve(); ok {
		t.Error("missing $OUTPOST_BIN should resolve false")
	}
	// A real executable resolves.
	bin := filepath.Join(t.TempDir(), "outpost")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BIN", bin)
	got, ok := Resolve()
	if !ok || got != bin {
		t.Errorf("Resolve() = %q,%v; want %q,true", got, ok, bin)
	}
	if !Installed() {
		t.Error("Installed() = false, want true")
	}
}
