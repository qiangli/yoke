package engine

import (
	"testing"
)

func TestContainerConfig(t *testing.T) {
	cfg := &ContainerConfig{
		Name:     "test-container",
		Image:    "ubuntu:24.04",
		ReadOnly: true,
		Init:     true,
		CapDrop:  []string{"ALL"},
		Tmpfs:    []string{"/tmp", "/var/tmp"},
		Env: map[string]string{
			"FOO": "bar",
		},
		Mounts: []Mount{
			{Source: "/host/path", Target: "/container/path", ReadOnly: true},
		},
		Labels: map[string]string{
			"ycode.session": "test-session",
		},
		Resources: Resources{
			CPUs:   "2.0",
			Memory: "4g",
		},
	}

	if cfg.Name != "test-container" {
		t.Error("unexpected name")
	}
	if !cfg.ReadOnly {
		t.Error("expected read-only")
	}
	if !cfg.Init {
		t.Error("expected init")
	}
	if len(cfg.CapDrop) != 1 || cfg.CapDrop[0] != "ALL" {
		t.Error("unexpected cap drop")
	}
	if len(cfg.Tmpfs) != 2 {
		t.Error("expected 2 tmpfs mounts")
	}
	if cfg.Env["FOO"] != "bar" {
		t.Error("unexpected env")
	}
	if len(cfg.Mounts) != 1 || !cfg.Mounts[0].ReadOnly {
		t.Error("unexpected mounts")
	}
}

func TestExecResult(t *testing.T) {
	r := &ExecResult{
		Stdout:   "hello world",
		Stderr:   "",
		ExitCode: 0,
	}

	if r.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", r.ExitCode)
	}
}
