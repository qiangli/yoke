// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"strings"
	"testing"
)

// TestUseEmbeddedServe_FailsClosedWithoutRunner verifies the supply-chain
// policy (Sprint 434, docs/bashy-service-supply-chain-policy.md): when the
// embedded runner is not compiled in, `ollama serve` must NOT fall back to a
// resolved system ollama or a downloaded archive. It must fail closed with a
// policy error, using only the embedded server.
func TestUseEmbeddedServe_FallsBackWithoutRunner(t *testing.T) {
	defer func(prev func() bool) { runnerAvailable = prev }(runnerAvailable)
	runnerAvailable = func() bool { return false } // build without -tags embed_runner

	opts := CmdOptions{RunEmbeddedServe: func(context.Context) error { return nil }}
	if useEmbeddedServe(opts) {
		t.Fatal("serve chose the embedded engine with no inference runner compiled in; per supply-chain policy it must not use embedded (and must fail closed, never fall back to PATH/download)")
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

// TestUseEmbeddedServe_SystemBinariesBypassesEmbedded keeps the explicit
// --use-system-binaries contract: it never runs the embedded engine.
func TestUseEmbeddedServe_SystemBinariesBypassesEmbedded(t *testing.T) {
	opts := CmdOptions{UseSystemBinaries: true, RunEmbeddedServe: func(context.Context) error { return nil }}
	if useEmbeddedServe(opts) {
		t.Fatal("--use-system-binaries must bypass the embedded engine")
	}
}

// TestUseEmbeddedServe_NoCallbackNeverEmbedded guards the nil-callback case.
func TestUseEmbeddedServe_NoCallbackNeverEmbedded(t *testing.T) {
	if useEmbeddedServe(CmdOptions{}) {
		t.Fatal("no RunEmbeddedServe callback: serve cannot take the embedded path")
	}
}

// TestServeFailsClosedWithoutRunner proves the gate: `ollama serve` without an
// embedded runner must fail with a supply-chain policy error, never invoke
// Resolve or exec a third-party binary, and never attempt a download (no network).
func TestServeFailsClosedWithoutRunner(t *testing.T) {
	defer func(prev func() bool) { runnerAvailable = prev }(runnerAvailable)
	runnerAvailable = func() bool { return false }

	cmd := NewOllamaCmd(CmdOptions{RunEmbeddedServe: func(context.Context) error { return nil }})
	cmd.SetArgs([]string{"serve"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected serve to fail closed per supply-chain policy, but succeeded")
	}
	if !strings.Contains(err.Error(), ollamaSupplyChainPolicy) && !strings.Contains(err.Error(), "supply-chain") {
		t.Fatalf("serve error = %q, want it to mention the supply-chain policy %q", err.Error(), ollamaSupplyChainPolicy)
	}
}

// TestRunFailsClosedWithoutDelegate proves `ollama run` without a delegate
// (the old PATH fallback) now fails closed per policy.
func TestRunFailsClosedWithoutDelegate(t *testing.T) {
	cmd := NewOllamaCmd(CmdOptions{})
	cmd.SetArgs([]string{"run", "llama3"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected run to fail closed per supply-chain policy, but succeeded")
	}
	if !strings.Contains(err.Error(), ollamaSupplyChainPolicy) && !strings.Contains(err.Error(), "supply-chain") {
		t.Fatalf("run error = %q, want policy %q", err.Error(), ollamaSupplyChainPolicy)
	}
}

// TestVersionFailsClosedWithoutDelegate proves `ollama version` without a
// delegate now fails closed per policy.
func TestVersionFailsClosedWithoutDelegate(t *testing.T) {
	cmd := NewOllamaCmd(CmdOptions{})
	cmd.SetArgs([]string{"version"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected version to fail closed per supply-chain policy, but succeeded")
	}
	if !strings.Contains(err.Error(), ollamaSupplyChainPolicy) && !strings.Contains(err.Error(), "supply-chain") {
		t.Fatalf("version error = %q, want policy %q", err.Error(), ollamaSupplyChainPolicy)
	}
}
