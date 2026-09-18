// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package clang

import (
	"regexp"
	"testing"
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestWinAssetPinned guards the Windows toolchain pin: llvm-mingw publishes no
// checksum file, so a present, well-formed sha256 is the trust boundary — a
// blank or malformed pin must fail the build, not silently skip verification.
func TestWinAssetPinned(t *testing.T) {
	a, ok := winAsset["windows/amd64"]
	if !ok {
		t.Fatal("windows/amd64 toolchain not pinned")
	}
	if a.arch != "x86_64" {
		t.Errorf("arch = %q, want x86_64", a.arch)
	}
	if !sha256Re.MatchString(a.sha256) {
		t.Errorf("sha256 = %q, want 64 lowercase hex chars (the verify is the trust boundary)", a.sha256)
	}
}
