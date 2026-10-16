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
