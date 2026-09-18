// Package vfkit_embed provides a self-extracting embedded vfkit binary.
// vfkit is the Apple Virtualization Framework helper that podman machine
// uses to run Linux VMs on macOS. It's ~5MB compressed to ~2MB.
//
// Build with: make vfkit-embed
package vfkit_embed

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

const binaryName = "vfkit"

// compressedVfkit holds the gzip-compressed vfkit binary.
var compressedVfkit []byte

// Available returns true if an embedded vfkit binary is present.
func Available() bool {
	return len(compressedVfkit) > 0
}

// EnsureVfkit extracts the embedded vfkit binary to cacheDir if not already
// present or if the embedded version has changed.
func EnsureVfkit(cacheDir string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("no embedded vfkit (build with: make vfkit-embed)")
	}

	binPath := filepath.Join(cacheDir, binaryName)
	hashPath := binPath + ".sha256"

	h := sha256.Sum256(compressedVfkit)
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

	gr, err := gzip.NewReader(bytes.NewReader(compressedVfkit))
	if err != nil {
		return "", fmt.Errorf("decompress vfkit: %w", err)
	}
	defer gr.Close()

	tmpPath := binPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("create vfkit binary: %w", err)
	}
	if _, err := io.Copy(f, gr); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write vfkit binary: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, binPath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	if runtime.GOOS == "darwin" {
		// vfkit talks to Apple's Virtualization.framework, which refuses
		// to construct a VZVirtualMachineConfiguration unless the calling
		// process holds `com.apple.security.virtualization`. Without it
		// `machine start` dies with VZErrorDomain Code=2:
		//   "Invalid virtual machine configuration. The process doesn't
		//   have the com.apple.security.virtualization entitlement."
		// Sign ad-hoc but WITH the entitlements vfkit upstream ships in
		// `vf.entitlements`.
		signVfkit(binPath)
	}

	os.WriteFile(hashPath, []byte(embeddedHash), 0o644) //nolint:errcheck
	return binPath, nil
}

// vfkitEntitlements is the plist vfkit upstream embeds via `make codesign`.
// Mirrored verbatim from github.com/crc-org/vfkit `vf.entitlements`.
const vfkitEntitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>com.apple.security.network.server</key>
	<true/>
	<key>com.apple.security.network.client</key>
	<true/>
	<key>com.apple.security.virtualization</key>
	<true/>
</dict>
</plist>
`

// signVfkit ad-hoc-signs the extracted vfkit with the Virtualization
// entitlement. Failure is non-fatal: if codesign is missing or returns
// an error, the user gets the same VZError on next start and can debug
// from there.
func signVfkit(binPath string) {
	ent, err := os.CreateTemp("", "vfkit-entitlements-*.plist")
	if err != nil {
		return
	}
	defer os.Remove(ent.Name())
	if _, err := ent.WriteString(vfkitEntitlements); err != nil {
		ent.Close()
		return
	}
	ent.Close()
	exec.Command("codesign", "--force", "--entitlements", ent.Name(), "--sign", "-", binPath).Run() //nolint:errcheck
}
