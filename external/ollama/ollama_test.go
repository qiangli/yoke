// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolve_EnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "ollama")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_BIN", fake)
	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != fake {
		t.Fatalf("Resolve = %q, want %q", got, fake)
	}
}

func TestResolve_EnvOverrideNonExecutable(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "notexec")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_BIN", bad)
	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve: want error for non-executable OLLAMA_BIN, got nil")
	}
}

func TestRun_NotFoundReturns127(t *testing.T) {
	t.Setenv("OLLAMA_BIN", filepath.Join(t.TempDir(), "does-not-exist"))
	var stderr strings.Builder
	code := Run(context.Background(), []string{"version"}, nil, nil, &stderr)
	if code != 127 {
		t.Fatalf("Run exit = %d, want 127", code)
	}
	if !strings.Contains(stderr.String(), "ollama") {
		t.Fatalf("stderr = %q, want it to mention ollama", stderr.String())
	}
}
