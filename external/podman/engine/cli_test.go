package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSafePathPodmanRejectsBashyShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "podman")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec bashy oci \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if _, err := safePathPodman(); err == nil || !strings.Contains(err.Error(), "bashy oci") {
		t.Fatalf("got %v", err)
	}
}

func TestResolvePodmanBinaryCachedEngine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", root)
	t.Setenv("BASHY_OFFLINE", "1")
	t.Setenv("BASHY_PODMAN_SYSTEM", "")
	t.Setenv("PATH", t.TempDir())
	bin := filepath.Join(root, "podman", "v6.1.2", "podman-"+runtime.GOOS+"-"+runtime.GOARCH,
		"usr", "local", "bin", "podman")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("cached engine"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePodmanBinary()
	if err != nil || got != bin {
		t.Fatalf("resolvePodmanBinary() = %q, %v; want %q", got, err, bin)
	}
}

func TestResolvePodmanBinaryIgnoresPathUnlessOptedIn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable fixture uses Unix mode")
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_OFFLINE", "1")
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BASHY_PODMAN_SYSTEM", "")
	if got, err := resolvePodmanBinary(); err == nil || got != "" || !strings.Contains(err.Error(), "no cached engine") {
		t.Fatalf("without opt-in: %q, %v", got, err)
	}
	t.Setenv("BASHY_PODMAN_SYSTEM", "1")
	if got, err := resolvePodmanBinary(); err != nil || got != bin {
		t.Fatalf("with opt-in: %q, %v; want %q", got, err, bin)
	}
}

func TestManagedExecLinuxHelpers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux helper layout")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "usr", "local", "bin")
	helpers := filepath.Join(dir, "usr", "local", "lib", "podman")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(helpers, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := buildManagedExec(context.Background(), filepath.Join(binDir, "podman"), "", nil)
	want := "CONTAINERS_HELPER_BINARY_DIR=" + helpers
	for _, e := range cmd.Env {
		if e == want {
			return
		}
	}
	t.Fatalf("missing %s", want)
}

func TestShouldEnsureMachine(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"default", nil, true},
		{"run", []string{"run", "alpine"}, true},
		{"info", []string{"info"}, true},
		{"help flag", []string{"--help"}, false},
		{"version flag", []string{"--version"}, false},
		{"machine", []string{"machine", "list"}, false},
		{"help command", []string{"help", "run"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldEnsureMachine(tt.args); got != tt.want {
				t.Fatalf("shouldEnsureMachine(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
