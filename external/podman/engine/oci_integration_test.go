//go:build integration

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOCIBuildSelf builds the ycode project inside its own embedded podman.
// This validates the full containerized build pipeline and is the foundation
// for the self-healing loop: ycode can build, test, and fix itself.
func TestOCIBuildSelf(t *testing.T) {
	skipIfNoPodman(t)

	// Find the ycode repo root.
	repoRoot, err := gitToplevel()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	// Verify the Dockerfile exists.
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	engine, err := NewEngine(ctx, &EngineConfig{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close(ctx)

	// Build the image from the repo's Dockerfile.
	imageName := "ycode-oci-selfbuild-test"
	t.Logf("building image %s from %s", imageName, repoRoot)
	if err := engine.BuildImageWithContext(ctx, imageName, dockerfile, repoRoot); err != nil {
		t.Fatalf("build image: %v", err)
	}

	// Create a container and run compile.
	ctr, err := engine.CreateContainer(ctx, &ContainerConfig{
		Name:    "oci-selfbuild-test",
		Image:   imageName,
		WorkDir: "/src",
		Command: []string{"sleep", "infinity"},
		Labels: map[string]string{
			"ycode.test": "oci-selfbuild",
		},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer ctr.Remove(ctx, true) //nolint:errcheck

	if err := ctr.Start(ctx); err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer ctr.Stop(ctx, 10*time.Second) //nolint:errcheck

	// Run make compile inside the container.
	t.Log("running make compile inside container")
	result, err := ctr.Exec(ctx, "make compile", "/src")
	if err != nil {
		t.Fatalf("exec make compile: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("make compile failed (exit %d):\nstdout: %s\nstderr: %s",
			result.ExitCode, result.Stdout, result.Stderr)
	}
	t.Logf("compile succeeded:\n%s", result.Stdout)

	// Verify the binary was built and can report its version.
	t.Log("verifying bin/ycode version")
	vResult, err := ctr.Exec(ctx, "bin/ycode version", "/src")
	if err != nil {
		t.Fatalf("exec version: %v", err)
	}
	if vResult.ExitCode != 0 {
		t.Fatalf("ycode version failed (exit %d): %s", vResult.ExitCode, vResult.Stderr)
	}
	if !strings.Contains(vResult.Stdout, "ycode") {
		t.Errorf("expected version output to contain 'ycode', got: %s", vResult.Stdout)
	}
	t.Logf("version: %s", vResult.Stdout)
}

// gitToplevel returns the repository root via git rev-parse.
func gitToplevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
