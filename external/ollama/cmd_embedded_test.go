// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"testing"
)

// TestUseEmbeddedServe_FallsBackWithoutRunner pins the regression where
// `ollama serve` (the argv the host model door spawns as its engine) ran the
// in-process embedded engine even on a from-source build with no inference
// runner compiled in, hard-failing with ErrRunnerNotInstalled and leaving the
// door unable to serve ANY local model. serve must instead fall back to a
// resolved system ollama when the runner is absent.
func TestUseEmbeddedServe_FallsBackWithoutRunner(t *testing.T) {
	defer func(prev func() bool) { runnerAvailable = prev }(runnerAvailable)
	runnerAvailable = func() bool { return false } // build without -tags embed_runner

	opts := CmdOptions{RunEmbeddedServe: func(context.Context) error { return nil }}
	if useEmbeddedServe(opts) {
		t.Fatal("serve chose the embedded engine with no inference runner compiled in; it must fall back to a resolved system ollama so the model door can still serve local models")
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
