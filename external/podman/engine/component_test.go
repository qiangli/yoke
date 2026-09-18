package engine

import (
	"testing"
)

func TestComponentConfig(t *testing.T) {
	cfg := &ComponentConfig{
		Enabled:      true,
		Image:        "ycode-sandbox:latest",
		Network:      "bridge",
		ReadOnlyRoot: true,
		PoolSize:     3,
		CPUs:         "2.0",
		Memory:       "4g",
	}

	if !cfg.Enabled {
		t.Error("expected enabled")
	}
	if cfg.Image != "ycode-sandbox:latest" {
		t.Error("unexpected image")
	}
}

func TestNewContainerComponent(t *testing.T) {
	cfg := &ComponentConfig{
		Enabled: true,
	}
	comp := NewContainerComponent(cfg, "/tmp/test-container")

	if comp.Name() != "containers" {
		t.Errorf("unexpected name: %s", comp.Name())
	}
	if comp.Healthy() {
		t.Error("should not be healthy before start")
	}
	if comp.SessionID() == "" {
		t.Error("expected non-empty session ID")
	}
}

func TestContainerComponentDefaults(t *testing.T) {
	cfg := &ComponentConfig{}
	comp := NewContainerComponent(cfg, "/tmp/test")

	// Should set default image and network.
	if cfg.Image != "ycode-sandbox:latest" {
		t.Errorf("expected default image, got %q", cfg.Image)
	}
	if cfg.Network != "bridge" {
		t.Errorf("expected default network, got %q", cfg.Network)
	}

	_ = comp
}

func TestServiceEnv(t *testing.T) {
	cfg := &ComponentConfig{
		Enabled: true,
	}
	comp := NewContainerComponent(cfg, "/tmp/test")
	comp.SetServicePorts(11434, 4317, 31415)

	// ServiceEnv returns nil when engine is nil (not started).
	env := comp.ServiceEnv()
	if env != nil {
		t.Error("expected nil env before engine init")
	}
}

func TestDefaultContainerConfig(t *testing.T) {
	cfg := &ComponentConfig{
		Enabled:      true,
		Image:        "test-image:v1",
		ReadOnlyRoot: true,
		CPUs:         "1.5",
		Memory:       "2g",
	}
	comp := NewContainerComponent(cfg, "/tmp/test")
	comp.SetServicePorts(11434, 4317, 31415)

	// Test default container config structure (before engine start).
	dcfg := comp.defaultContainerConfig()
	if dcfg.Image != "test-image:v1" {
		t.Errorf("unexpected image: %s", dcfg.Image)
	}
	if !dcfg.ReadOnly {
		t.Error("expected read-only root")
	}
	if !dcfg.Init {
		t.Error("expected init flag")
	}
	if len(dcfg.CapDrop) != 1 || dcfg.CapDrop[0] != "ALL" {
		t.Error("expected cap drop ALL")
	}
	if len(dcfg.Tmpfs) != 3 {
		t.Errorf("expected 3 tmpfs mounts, got %d", len(dcfg.Tmpfs))
	}
	if dcfg.CPUs != "1.5" {
		t.Errorf("unexpected CPUs: %s", dcfg.CPUs)
	}
	if dcfg.Memory != "2g" {
		t.Errorf("unexpected Memory: %s", dcfg.Memory)
	}
	if dcfg.Labels[SessionLabel] == "" {
		t.Error("expected session label")
	}
}
