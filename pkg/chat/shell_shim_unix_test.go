//go:build !windows

package chat

import (
	"io"
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

func TestAdapterProcessUsesBashyLauncher(t *testing.T) {
	if os.Getenv("CHAT_SHIM_ADAPTER_CHILD") == "1" {
		executable, err := os.Executable()
		if err != nil || !adapterCacheTarget(executable) {
			t.Fatalf("adapter executable = %q, %v", executable, err)
		}
		launcher, err := agentShellLauncher()
		if err != nil || launcher != os.Getenv("BASHY_SELF") {
			t.Fatalf("launcher = %q, %v", launcher, err)
		}
		dir := ensureShims(launcher)
		if dir == "" {
			t.Fatal("shims not installed")
		}
		for _, name := range []string{"sh", "bash", "zsh"} {
			out, err := exec.Command(filepath.Join(dir, name), "hello").CombinedOutput()
			if err != nil || string(out) != "launcher:hello\n" {
				t.Fatalf("%s: %q, %v", name, out, err)
			}
		}
		if got := ensureShims(executable); got != "" {
			t.Fatalf("accepted adapter: %q", got)
		}
		return
	}
	cache := t.TempDir()
	adapter := filepath.Join(cache, "foreman")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(adapter, os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy adapter: %v, %v", copyErr, closeErr)
	}
	launcher := filepath.Join(t.TempDir(), "bashy")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nprintf 'launcher:%s\n' \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHAT_SHIM_ADAPTER_CHILD", "1")
	t.Setenv("BASHY_ADAPTER_CACHE", cache)
	t.Setenv("BASHY_SELF", launcher)
	out, err := exec.Command(adapter, "-test.run=^TestAdapterProcessUsesBashyLauncher$", "-test.v").CombinedOutput()
	if err != nil {
		t.Fatalf("adapter child: %v\n%s", err, out)
	}
	t.Log(string(out))
}

func TestShellShimRejectsAdapterCaches(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	override := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", xdg)
	t.Setenv("BASHY_ADAPTER_CACHE", override)
	shim := filepath.Join(t.TempDir(), "sh")
	for _, root := range []string{override, filepath.Join(xdg, "bashy", "adapters"), filepath.Join(home, ".cache", "bashy", "adapters")} {
		target := filepath.Join(root, "hash", "foreman")
		if err := WriteShellShim(shim, target); err == nil {
			t.Errorf("accepted %q", target)
		}
		if _, err := os.Stat(shim); !os.IsNotExist(err) {
			t.Fatalf("rejected target wrote shim: %v", err)
		}
	}
	target := filepath.Join(override, "foreman")
	if err := os.WriteFile(target, nil, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "bashy")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if err := WriteShellShim(shim, alias); err == nil {
		t.Fatal("accepted symlink to adapter")
	}
	if err := WriteShellShim(shim, override+"-outside/bashy"); err != nil {
		t.Fatalf("rejected sibling: %v", err)
	}
}

func TestAgentShellLauncherPATH(t *testing.T) {
	dir := t.TempDir()
	launcher := filepath.Join(dir, "bashy")
	if err := os.WriteFile(launcher, nil, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BASHY_SELF", "")
	got, err := agentShellLauncher()
	if err != nil || got != launcher {
		t.Fatalf("launcher = %q, %v", got, err)
	}
	t.Setenv("BASHY_ADAPTER_CACHE", dir)
	if _, err := agentShellLauncher(); err == nil {
		t.Fatal("accepted PATH adapter")
	}
	t.Setenv("BASHY_SELF", launcher)
	if _, err := agentShellLauncher(); err == nil {
		t.Fatal("accepted BASHY_SELF adapter")
	}
	t.Setenv("BASHY_SELF", "")
	t.Setenv("PATH", t.TempDir())
	if _, err := agentShellLauncher(); err == nil {
		t.Fatal("accepted missing launcher")
	}
}
