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

	"github.com/qiangli/yoke/pkg/broker/door"
)

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
	if err := cmd.Execute(); err != nil {
		t.Fatalf("system serve: %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if host := string(got); host != "127.0.0.1:11501" {
		t.Fatalf("system OLLAMA_HOST = %q, want managed bind", host)
	}
}

func TestManagedServeHonorsUserHost(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11500")
	if got := applyManagedEnv(managedPort()); got != "http://127.0.0.1:11500" {
		t.Fatalf("applyManagedEnv = %q, want user host", got)
	}
}
