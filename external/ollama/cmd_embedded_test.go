// Copyright (c) 2026 qiangli
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

// TestManagedServeRefusesSystemFallbackWithoutRunner pins the service supply-
// chain boundary: neither OLLAMA_BIN nor PATH may turn into an executable
// fallback when the bashy-signed embedded runner is absent.
func TestManagedServeRefusesSystemFallbackWithoutRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake executable is a Unix shell script")
	}
	defer func(prev func() bool) { runnerAvailable = prev }(runnerAvailable)
	runnerAvailable = func() bool { return false }

	for _, source := range []string{"OLLAMA_BIN", "PATH"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "invoked")
			fake := filepath.Join(dir, "ollama")
			if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf invoked > \""+marker+"\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if source == "OLLAMA_BIN" {
				t.Setenv("OLLAMA_BIN", fake)
				t.Setenv("PATH", "")
			} else {
				t.Setenv("OLLAMA_BIN", "")
				t.Setenv("PATH", dir)
			}

			cmd := NewManagedOllamaCmd()
			cmd.SetArgs([]string{"serve"})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("managed serve succeeded without a bashy-signed embedded runner")
			}
			for _, want := range []string{"bashy-signed embedded runner", "refusing", "OLLAMA_BIN", "PATH"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("managed serve error %q does not contain %q", err, want)
				}
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("unverified %s executable was invoked (marker error: %v)", source, statErr)
			}
		})
	}
}

// TestUseEmbeddedServe_EmbeddedWhenRunnerPresent is the companion: with a
// runner compiled in and no --use-system-binaries, serve runs the embedded
// engine.
func TestUseEmbeddedServe_EmbeddedWhenRunnerPresent(t *testing.T) {
	defer func(prev func() bool) { runnerAvailable = prev }(runnerAvailable)
	runnerAvailable = func() bool { return true }

	opts := CmdOptions{RunEmbeddedServe: func(context.Context) error { return nil }}
	if !useEmbeddedServe(opts) {
		t.Fatal("with an embedded runner and no --use-system-binaries, serve must use the embedded engine")
	}
}

// TestManagedServeRefusesUseSystemBinaries ensures the explicit system mode
// cannot bypass the managed-service supply-chain boundary.
func TestManagedServeRefusesUseSystemBinaries(t *testing.T) {
	defer func(prev func() bool) { runnerAvailable = prev }(runnerAvailable)
	runnerAvailable = func() bool { return true }

	cmd := NewOllamaCmd(CmdOptions{
		UseSystemBinaries: true,
		RunEmbeddedServe:  func(context.Context) error { t.Fatal("embedded serve called"); return nil },
	})
	cmd.SetArgs([]string{"serve"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "refusing --use-system-binaries") {
		t.Fatalf("managed --use-system-binaries error = %v, want clear refusal", err)
	}
}

// TestUseEmbeddedServe_NoCallbackNeverEmbedded guards the nil-callback case.
func TestUseEmbeddedServe_NoCallbackNeverEmbedded(t *testing.T) {
	if useEmbeddedServe(CmdOptions{}) {
		t.Fatal("no RunEmbeddedServe callback: serve cannot take the embedded path")
	}
}
