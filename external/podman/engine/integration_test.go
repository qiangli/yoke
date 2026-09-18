//go:build integration

package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

func skipIfNoPodman(t *testing.T) {
	t.Helper()
	// Check for a running podman socket — ycode IS podman, so we don't
	// look for an external binary. The socket is provided by either
	// the in-process service (Linux) or a podman machine (macOS).
	if sp := defaultSocketPath(); sp == "" || !socketAvailable(sp) {
		t.Skip("no podman socket available, skipping integration test")
	}
}

func TestIntegrationEngineConnect(t *testing.T) {
	skipIfNoPodman(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	engine, err := NewEngine(ctx, &EngineConfig{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close(ctx)

	if !engine.Healthy() {
		t.Error("engine should be healthy after init")
	}

	version, err := engine.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version == "" {
		t.Error("expected non-empty version")
	}
	t.Logf("podman version: %s", version)
}

func TestIntegrationImageOperations(t *testing.T) {
	skipIfNoPodman(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	engine, err := NewEngine(ctx, &EngineConfig{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close(ctx)

	// Pull a tiny image.
	testImage := "docker.io/library/alpine:latest"
	if err := engine.PullImage(ctx, testImage); err != nil {
		t.Fatalf("PullImage: %v", err)
	}

	// Verify it exists.
	if !engine.ImageExists(ctx, testImage) {
		t.Error("image should exist after pull")
	}

	// List images.
	images, err := engine.ListImages(ctx)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) == 0 {
		t.Error("expected at least one image")
	}
	t.Logf("found %d images", len(images))
}

func TestIntegrationContainerLifecycle(t *testing.T) {
	skipIfNoPodman(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	engine, err := NewEngine(ctx, &EngineConfig{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close(ctx)

	// Ensure image exists.
	testImage := "docker.io/library/alpine:latest"
	if err := engine.EnsureImage(ctx, testImage); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	// Create container with a long-running command via REST API.
	containerName := "ycode-test-lifecycle"

	// Remove any leftover from previous run.
	leftover := NewContainer(engine, containerName)
	leftover.Remove(ctx, true) // ignore error

	cfg := &ContainerConfig{
		Name:    containerName,
		Image:   testImage,
		Command: []string{"sleep", "300"},
		Labels: map[string]string{
			SessionLabel: "integration-test",
		},
	}

	ctr, err := engine.CreateContainer(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	defer ctr.Remove(ctx, true)

	if ctr.ID == "" {
		t.Error("expected non-empty container ID")
	}

	if err := ctr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Check running.
	if !ctr.IsRunning(ctx) {
		t.Error("container should be running")
	}

	// Exec a command.
	result, err := ctr.Exec(ctx, "echo hello-from-container", "")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello-from-container") {
		t.Errorf("unexpected exec output: %q", result.Stdout)
	}

	// Stop container.
	if err := ctr.Stop(ctx, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Should no longer be running.
	if ctr.IsRunning(ctx) {
		t.Error("container should not be running after stop")
	}
}

func TestIntegrationNetworkOperations(t *testing.T) {
	skipIfNoPodman(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	engine, err := NewEngine(ctx, &EngineConfig{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close(ctx)

	networkName := "ycode-test-network"

	// Clean up first.
	engine.RemoveNetwork(ctx, networkName)

	// Create network.
	if err := engine.CreateNetwork(ctx, networkName); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	defer engine.RemoveNetwork(ctx, networkName)

	// List networks.
	networks, err := engine.ListNetworks(ctx, networkName)
	if err != nil {
		t.Fatalf("ListNetworks: %v", err)
	}

	found := false
	for _, n := range networks {
		if n.Name == networkName {
			found = true
			break
		}
	}
	if !found {
		t.Error("created network not found in list")
	}

	// Creating the same network again should not error (idempotent).
	if err := engine.CreateNetwork(ctx, networkName); err != nil {
		t.Fatalf("CreateNetwork (idempotent): %v", err)
	}
}

func TestIntegrationContainerList(t *testing.T) {
	skipIfNoPodman(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	engine, err := NewEngine(ctx, &EngineConfig{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close(ctx)

	// List all containers (just verify no error).
	containers, err := engine.ListContainers(ctx, nil)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	t.Logf("found %d containers", len(containers))
}

func TestIntegrationRunOneShot(t *testing.T) {
	skipIfNoPodman(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	engine, err := NewEngine(ctx, &EngineConfig{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close(ctx)

	// Happy path: alpine echoes to stdout, exit 0, separate streams captured.
	res, err := engine.RunOneShot(ctx, &ContainerConfig{
		Image:   "alpine:3.21",
		Network: "none",
		Command: []string{"sh", "-c", "echo out; echo err 1>&2; exit 7"},
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "out") {
		t.Errorf("stdout = %q, want to contain 'out'", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "err") {
		t.Errorf("stderr = %q, want to contain 'err'", res.Stderr)
	}
	// Cross-stream leakage check: stdout must not contain stderr content
	// (the demux is the whole point of returning separate strings).
	if strings.Contains(res.Stdout, "err") {
		t.Errorf("stdout leaked stderr content: %q", res.Stdout)
	}
	if strings.Contains(res.Stderr, "out") {
		t.Errorf("stderr leaked stdout content: %q", res.Stderr)
	}
}
