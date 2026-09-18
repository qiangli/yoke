// Package gvproxy_embed provides a self-extracting embedded gvproxy binary.
// gvproxy is containers/gvisor-tap-vsock — the user-mode network proxy that
// podman machine spawns to forward host sockets into the Linux VM. Both
// applehv and libkrun providers need it. Upstream podman ships gvproxy as
// a separate package (homebrew lists it as a runtime dependency); for a
// single-binary ycode we embed it the same way we embed vfkit.
//
// Build with: make gvproxy-embed
package gvproxy_embed

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

const binaryName = "gvproxy"

// compressedGvproxy holds the gzip-compressed gvproxy binary.
var compressedGvproxy []byte

// Available returns true if an embedded gvproxy binary is present.
func Available() bool {
	return len(compressedGvproxy) > 0
}

// EnsureGvproxy extracts the embedded gvproxy binary to cacheDir if not
// already present or if the embedded version has changed.
func EnsureGvproxy(cacheDir string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("no embedded gvproxy (build with: make gvproxy-embed)")
	}

	binPath := filepath.Join(cacheDir, binaryName)
	hashPath := binPath + ".sha256"

	h := sha256.Sum256(compressedGvproxy)
	embeddedHash := fmt.Sprintf("%x", h)

	if cachedHash, err := os.ReadFile(hashPath); err == nil {
		if string(cachedHash) == embeddedHash {
			if info, err := os.Stat(binPath); err == nil && info.Mode().IsRegular() {
				return binPath, nil
			}
		}
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	gr, err := gzip.NewReader(bytes.NewReader(compressedGvproxy))
	if err != nil {
		return "", fmt.Errorf("decompress gvproxy: %w", err)
	}
	defer gr.Close()

	tmpPath := binPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("create gvproxy binary: %w", err)
	}
	if _, err := io.Copy(f, gr); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write gvproxy binary: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, binPath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	// macOS Sequoia kernel rejects freshly produced unsigned binaries
	// (`load code signature error 2`). Mirroring vfkit_embed.
	if runtime.GOOS == "darwin" {
		exec.Command("codesign", "-f", "-s", "-", binPath).Run() //nolint:errcheck
	}

	os.WriteFile(hashPath, []byte(embeddedHash), 0o644) //nolint:errcheck
	return binPath, nil
}
