//go:build !windows

package chat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteShellShimRejectsTestBinary(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "sh")
	cases := []struct {
		name  string
		bashy string
	}{
		{"ends with .test", "/tmp/bashy.test"},
		{"go-build path with bashy", "/tmp/go-build123/b001/bashy"},
		{"go-build with test suffix", "/tmp/go-build123/b001/exe.test"},
		{"bashy in go-build", "/var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/go-build2697496073/b1342/agentos.test"},
	}
	for _, tc := range cases {
		if err := WriteShellShim(shim, tc.bashy); err == nil {
			t.Errorf("%s: WriteShellShim(%q) should fail", tc.name, tc.bashy)
		}
	}
	// valid names must succeed
	for _, bashy := range []string{"/tmp/bashy", "/tmp/bashy.exe", "/tmp/bin/bash"} {
		shim2 := filepath.Join(t.TempDir(), "sh")
		if err := WriteShellShim(shim2, bashy); err != nil {
			t.Errorf("WriteShellShim(%q) should succeed: %v", bashy, err)
		} else {
			b, err := os.ReadFile(shim2)
			if err != nil || !strings.Contains(string(b), bashy) {
				t.Errorf("shim for %q not written correctly", bashy)
			}
		}
	}
}

func TestEnsureShimsRejectsTestBinary(t *testing.T) {
	t.Setenv("BASHY_SHIM_DIR", filepath.Join(t.TempDir(), "shims"))
	// basename not bashy
	if got := ensureShims("/tmp/go-build123/b001/agentos.test"); got != "" {
		t.Fatalf("ensureShims with test binary should return empty, got %q", got)
	}
}

func TestShimIsolationDoesNotTouchRealHome(t *testing.T) {
	isolated := shimDir()
	if isolated == "" {
		t.Fatal("shimDir empty under isolation")
	}
	if isolatedRealShimDir == "" {
		t.Skip("no real shim dir captured")
	}
	if isolated == isolatedRealShimDir {
		t.Fatalf("isolation failed: shimDir %q still points to real home %q", isolated, isolatedRealShimDir)
	}
	// Snapshot real shim dir before any ensureShims call in this test process.
	snap := make(map[string][]byte)
	snapErr := make(map[string]error)
	for _, name := range []string{"sh", "bash", "zsh"} {
		p := filepath.Join(isolatedRealShimDir, name)
		b, err := os.ReadFile(p)
		snap[p] = b
		snapErr[p] = err
	}
	// Attempt to trigger shim creation with a test binary — it should be rejected
	// and must not modify the real HOME shim dir.
	_ = ensureShims("/tmp/go-build999/b001/agentos.test")
	_ = ensureShims("/tmp/notbashy")
	for _, name := range []string{"sh", "bash", "zsh"} {
		p := filepath.Join(isolatedRealShimDir, name)
		b, err := os.ReadFile(p)
		prev := snap[p]
		prevErr := snapErr[p]
		if (err == nil) != (prevErr == nil) {
			t.Errorf("real HOME shim %q existence changed by isolated test (before err %v, after err %v)", p, prevErr, err)
		}
		if err == nil && prevErr == nil && string(b) != string(prev) {
			t.Errorf("real HOME shim %q was modified by isolated test", p)
		}
	}
	// Also verify that isolation itself is effective: ensureShims with a valid
	// isolated bashy writes to the isolated dir, not the real one.
	validBashy := filepath.Join(t.TempDir(), "bashy")
	if err := os.WriteFile(validBashy, []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := ensureShims(validBashy); got != isolated {
		t.Fatalf("ensureShims with isolated bashy should return isolated dir %q, got %q", isolated, got)
	}
}

func TestEnsureShimsPreservesAgentOSEntryPoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "bashy")
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
