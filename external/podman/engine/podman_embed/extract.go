// Package podman_embed provides a self-extracting embedded Podman binary.
// The podman binary is compressed with gzip and optionally embedded via go:embed.
// On first use, it extracts to a cache directory and reuses the cached binary
// on subsequent runs (validated by SHA256 hash).
//
// Build the embedded binary with: make podman-embed
package podman_embed

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

const binaryName = "podman"

// compressedPodman holds the gzip-compressed podman binary.
// Set by embed_podman.go (build tag) or empty via embed_stub.go.
var compressedPodman []byte

// Available returns true if an embedded podman binary is present.
func Available() bool {
	return len(compressedPodman) > 0
}

// EnsurePodman extracts the embedded podman binary to cacheDir if not already
// present or if the embedded version has changed (SHA256 mismatch).
// Returns the absolute path to the executable binary.
func EnsurePodman(cacheDir string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("no embedded podman (build with: make podman-embed)")
	}

	binName := binaryName
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(cacheDir, binName)
	hashPath := binPath + ".sha256"

	// Compute hash of embedded binary.
	h := sha256.Sum256(compressedPodman)
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

	gr, err := gzip.NewReader(bytes.NewReader(compressedPodman))
	if err != nil {
		return "", fmt.Errorf("decompress podman: %w", err)
	}
	defer gr.Close()

	// Atomic write: temp file → rename.
	tmpPath := binPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("create podman binary: %w", err)
	}

	if _, err := io.Copy(f, gr); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write podman binary: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, binPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("install podman binary: %w", err)
	}

	// macOS: ad-hoc codesign.
	if runtime.GOOS == "darwin" {
		cmd := exec.Command("codesign", "-f", "-s", "-", binPath)
		cmd.Run() //nolint:errcheck
	}

	os.WriteFile(hashPath, []byte(embeddedHash), 0o644) //nolint:errcheck
	return binPath, nil
}
