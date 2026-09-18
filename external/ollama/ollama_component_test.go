// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewOllamaComponent(t *testing.T) {
	cfg := &Config{}
	comp := NewOllamaComponent(cfg, "/tmp/test-data")

	if comp.Name() != "ollama" {
		t.Errorf("Name() = %q, want %q", comp.Name(), "ollama")
	}
	if comp.Healthy() {
		t.Error("new component should not be healthy")
	}
	if comp.Port() != 0 {
		t.Errorf("Port() = %d, want 0", comp.Port())
	}
	if comp.BaseURL() != "" {
		t.Errorf("BaseURL() = %q, want empty", comp.BaseURL())
	}
	if comp.HTTPHandler() != nil {
		t.Error("HTTPHandler() should be nil before start")
	}
}

func TestOllamaComponent_Prepare_CreatesDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "inference")
	comp := NewOllamaComponent(&Config{}, dir)

	if err := comp.prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("data directory was not created")
	}
}

func TestOllamaComponent_Stop_BeforeStart(t *testing.T) {
	cfg := &Config{}
	comp := NewOllamaComponent(cfg, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := comp.Stop(ctx); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}

func TestOllamaComponent_Prepare_ModelsDir_EnvVar(t *testing.T) {
	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	t.Setenv("OLLAMA_MODELS", "")
	cfg := &Config{ModelsDir: modelsDir}
	comp := NewOllamaComponent(cfg, dir)

	if err := comp.prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := os.Getenv("OLLAMA_MODELS"); got != modelsDir {
		t.Errorf("OLLAMA_MODELS = %q, want %q", got, modelsDir)
	}
}

func TestOllamaComponent_Start_UseSystem_NotReachable(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:1")

	tr := true
	cfg := &Config{UseSystem: &tr}
	comp := NewOllamaComponent(cfg, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := comp.Start(ctx)
	if err == nil {
		t.Fatal("Start should fail when useSystem=true and no daemon is reachable")
	}
	msg := err.Error()
	for _, want := range []string{"system mode", "--use-system-binaries"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message should mention %q for clarity; got: %v", want, err)
		}
	}
	if comp.Healthy() {
		t.Error("component should not be healthy after failed start")
	}
}

func TestOllamaComponent_BaseURL_UseSystem(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	tr := true
	cfg := &Config{UseSystem: &tr}
	comp := NewOllamaComponent(cfg, t.TempDir())

	if got := comp.BaseURL(); got == "" {
		t.Error("BaseURL should return system URL in useSystem mode, even before Start")
	}
}
