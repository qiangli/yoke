// Package runner_embed provides a self-extracting embedded inference runner.
// The runner binary (llama.cpp + thin HTTP server) is compressed with gzip
// and embedded via go:embed. On first use, it extracts to a cache directory
// and reuses the cached binary on subsequent runs (validated by SHA256 hash).
//
// This eliminates the need to install or download a separate ollama binary.
// Build the embedded runner with: make runner-build-thin
package runner_embed

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// runnerName is the extracted binary name.
const runnerName = "ycode-runner"

// compressedRunner holds the gzip-compressed runner binary.
// Set by embed_runner.go (when built with runner present) or
// embed_stub.go (when no runner is available).
var compressedRunner []byte

// Available returns true if an embedded runner binary is present.
// The runner may not be embedded in development builds.
func Available() bool {
	return len(compressedRunner) > 0
}

// EnsureRunner extracts the embedded runner binary to cacheDir if not already
// present or if the embedded version has changed (SHA256 mismatch).
// Returns the absolute path to the executable runner binary.
func EnsureRunner(cacheDir string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("no embedded runner (build with: make runner-build-thin)")
	}

	binName := runnerName
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(cacheDir, binName)
	hashPath := binPath + ".sha256"

	// Compute hash of embedded runner.
	h := sha256.Sum256(compressedRunner)
	embeddedHash := fmt.Sprintf("%x", h)

	// Check if cached version matches.
	if cachedHash, err := os.ReadFile(hashPath); err == nil {
		if string(cachedHash) == embeddedHash {
			if info, err := os.Stat(binPath); err == nil && info.Mode().IsRegular() {
				return binPath, nil // cached and up-to-date
			}
		}
	}

	// Extract: decompress → write binary → write hash.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir %s: %w", cacheDir, err)
	}

	gr, err := gzip.NewReader(bytes.NewReader(compressedRunner))
	if err != nil {
		return "", fmt.Errorf("decompress runner: %w", err)
	}
	defer gr.Close()

	// Write to temp file then rename for atomicity.
	tmpPath := binPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("create runner binary: %w", err)
	}

	if _, err := io.Copy(f, gr); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write runner binary: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, binPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("install runner binary: %w", err)
	}

	// macOS: ad-hoc codesign so Gatekeeper doesn't block it.
	if runtime.GOOS == "darwin" {
		codesign(binPath)
	}

	// Write hash for cache validation.
	os.WriteFile(hashPath, []byte(embeddedHash), 0o644) //nolint:errcheck

	return binPath, nil
}

// codesign performs ad-hoc signing on macOS (same pattern as make install).
func codesign(path string) {
	cmd := exec.Command("codesign", "-f", "-s", "-", path)
	cmd.Run() //nolint:errcheck
}
