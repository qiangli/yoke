// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/internal/serviceenv"
	"github.com/qiangli/yoke/pkg/broker/door"
)

func TestManagedServeEnvDisablesCloudAndPinsLocalOTLP(t *testing.T) {
	got := strings.Join(managedServeEnv([]string{
		"OLLAMA_NO_CLOUD=0",
		"OTEL_EXPORTER_OTLP_ENDPOINT=https://vendor.example",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://traces.vendor.example",
	}, 11435), "\n")
	if strings.Contains(got, "vendor.example") {
		t.Fatalf("vendor endpoint survived:\n%s", got)
	}
	for _, want := range []string{
		"OLLAMA_NO_CLOUD=1",
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + serviceenv.LocalCollectorEndpoint,
		"OTEL_SERVICE_NAME=ollama",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

func TestManagedServeDoesNotBindDoorHost(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_OLLAMA_PORT", "11501")
	doorHost, err := door.OllamaHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_HOST", doorHost)

	// Pretend the inference runner is compiled in so serve takes the embedded
	// path (this test observes its bind env, not a real runner).
	defer func(prev func() bool) { runnerAvailable = prev }(runnerAvailable)
	runnerAvailable = func() bool { return true }

	var embeddedHost string
	// Replace the embedded server so this test observes its bind environment
	// without starting a model server.
	cmd := NewOllamaCmd(CmdOptions{RunEmbeddedServe: func(context.Context) error {
		embeddedHost = os.Getenv("OLLAMA_HOST")
		return nil
	}})
	cmd.PersistentPreRun = func(c *cobra.Command, _ []string) {
		if c.Name() == "serve" {
			applyManagedEnv(managedPort())
		}
	}
	cmd.SetArgs([]string{"serve"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("embedded serve: %v", err)
	}
	want := "127.0.0.1:11501"
	if embeddedHost != want {
		t.Fatalf("embedded OLLAMA_HOST = %q, want managed bind %q", embeddedHost, want)
	}
	if strings.Contains(embeddedHost, ":"+strconv.Itoa(door.Port())) {
		t.Fatalf("embedded serve inherited door port: %q", embeddedHost)
	}
}

func TestSystemServeDoesNotBindDoorHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test binary is Unix-specific")
	}
	// Per docs/bashy-service-supply-chain-policy.md (Sprint 434) system
	// serve is disabled: `ollama serve` never execs a third-party binary.
	// This test now verifies it fails closed with a policy error, rather than
	// executing the binary and checking OLLAMA_HOST.
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_OLLAMA_PORT", "11501")
	doorHost, err := door.OllamaHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_HOST", doorHost)

	output := filepath.Join(t.TempDir(), "host")
	bin := filepath.Join(t.TempDir(), "ollama")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' \"$OLLAMA_HOST\" > "+output+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_BIN", bin)
	cmd := NewOllamaCmd(CmdOptions{UseSystemBinaries: true})
	cmd.SetArgs([]string{"serve"})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected system serve to fail closed per supply-chain policy, but succeeded")
	}
	if !strings.Contains(err.Error(), ollamaSupplyChainPolicy) && !strings.Contains(err.Error(), "supply-chain") {
		t.Fatalf("system serve error = %q, want policy %q", err.Error(), ollamaSupplyChainPolicy)
	}
	// Ensure the fake binary was NOT executed.
	if _, err := os.Stat(output); err == nil {
		data, _ := os.ReadFile(output)
		t.Fatalf("system binary was executed despite policy refusal; output %q", string(data))
	}
	// Also verify managedServeEnv still produces the correct env independently.
	env := managedServeEnv([]string{"OLLAMA_HOST=" + doorHost}, 11501)
	found := false
	for _, e := range env {
		if e == "OLLAMA_HOST=127.0.0.1:11501" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("managedServeEnv did not pin 127.0.0.1:11501: %v", env)
	}
}

func TestManagedServeHonorsUserHost(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11500")
	if got := applyManagedEnv(managedPort()); got != "http://127.0.0.1:11500" {
		t.Fatalf("applyManagedEnv = %q, want user host", got)
	}
}
