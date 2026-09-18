//go:build darwin

package chat

import "golang.org/x/sys/unix"

// stripQuarantine removes the macOS Gatekeeper com.apple.quarantine attribute
// from an operator-configured agent binary so a background/CI launch doesn't
// hang on the "downloaded from the Internet" popup. Best-effort: errors (not
// present, no permission) are ignored. Removexattr follows the symlink, so
// clearing via a PATH shim (…/bin/codex) clears the real binary it points to.
func stripQuarantine(path string) {
	_ = unix.Removexattr(path, "com.apple.quarantine")
}
