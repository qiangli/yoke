// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve_RefusesPerSupplyChainPolicy(t *testing.T) {
	// Even with OLLAMA_BIN set to an executable, Resolve must fail closed
	// per docs/bashy-service-supply-chain-policy.md (embedded only).
	dir := t.TempDir()
	fake := filepath.Join(dir, "ollama")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_BIN", fake)
	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve: want error per supply-chain policy, got nil")
	} else if !strings.Contains(err.Error(), ollamaSupplyChainPolicy) && !strings.Contains(err.Error(), "supply-chain") {
		t.Fatalf("Resolve error = %q, want it to mention the supply-chain policy %q", err.Error(), ollamaSupplyChainPolicy)
	} else if !strings.Contains(err.Error(), "732cbb22") {
		t.Fatalf("Resolve error = %q, want it to point at the signed embedded runner (story 732cbb22)", err.Error())
	}
}

func TestResolve_AlwaysFailsWithoutNetwork(t *testing.T) {
	t.Setenv("OLLAMA_BIN", "")
	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve: want error per supply-chain policy, got nil")
	} else if !strings.Contains(err.Error(), ollamaSupplyChainPolicy) {
		t.Fatalf("Resolve error = %q, want policy %q", err.Error(), ollamaSupplyChainPolicy)
	}
}

func TestRun_RefusesPerSupplyChainPolicy(t *testing.T) {
	t.Setenv("OLLAMA_BIN", filepath.Join(t.TempDir(), "does-not-exist"))
	var stderr strings.Builder
	code := Run(context.Background(), []string{"version"}, nil, nil, &stderr)
	if code != 127 {
		t.Fatalf("Run exit = %d, want 127", code)
	}
	if !strings.Contains(stderr.String(), ollamaSupplyChainPolicy) && !strings.Contains(stderr.String(), "supply-chain") {
		t.Fatalf("stderr = %q, want it to mention the supply-chain policy %q", stderr.String(), ollamaSupplyChainPolicy)
	}
	// Ensure no exec was attempted (no network, no PATH probe).
	if strings.Contains(stderr.String(), "on PATH") {
		t.Fatalf("stderr = %q, want no PATH fallback mention", stderr.String())
	}
}

// TestRun_EnvOverrideStillRefused proves OLLAMA_BIN does not enable a bypass:
// a live executable at OLLAMA_BIN must not be invoked.
func TestRun_EnvOverrideStillRefused(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ollama")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho should-not-run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_BIN", fake)
	var stderr, stdout strings.Builder
	code := Run(context.Background(), []string{"version"}, nil, &stdout, &stderr)
	if code != 127 {
		t.Fatalf("Run exit = %d, want 127 (policy refusal even with OLLAMA_BIN set)", code)
	}
	if !strings.Contains(stderr.String(), ollamaSupplyChainPolicy) {
		t.Fatalf("stderr = %q, want policy %q", stderr.String(), ollamaSupplyChainPolicy)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty: the OLLAMA_BIN binary must not have run", stdout.String())
	}
}
