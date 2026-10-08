//go:build !windows

package chat

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEnsureShimsPreservesAgentOSEntryPoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "bashy's shell")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nprintf '%s\\n' \"$0\"\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_SHIM_DIR", filepath.Join(t.TempDir(), "shims"))
	dir := shimDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "sh", "zsh"} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if got := ensureShims(target); got != dir {
		t.Fatalf("ensureShims = %q", got)
	}
	if got := ensureShims(target); got != dir {
		t.Fatalf("repeat ensureShims = %q", got)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != string(original) {
		t.Fatal("migration overwrote symlink target")
	}
	for _, name := range []string{"bash", "sh", "zsh"} {
		out, err := exec.Command(filepath.Join(dir, name), "-c", "a b").CombinedOutput()
		want := target + "\n-c\na b\n"
		if err != nil || string(out) != want {
			t.Errorf("%s: got %q, err %v; want %q", name, out, err, want)
		}
	}
}
