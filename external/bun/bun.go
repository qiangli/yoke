// Package bun provisions the Bun JavaScript runtime from its GitHub releases —
// the alternative TypeScript island runtime a project selects with a bun
// lockfile or BASHPP_TYPESCRIPT_RUNTIME=bun (node stays the default).
//
// License, as upstream's LICENSE.md states it: Bun's own code is MIT, but the
// shipped binary statically links JavaScriptCore/WebKit (LGPL-2) and tinycc
// (LGPL-2.1), so the program is NOT permissive as a whole; the release zip
// carries no license file. That is fine only because it is download + exec —
// a separate process on its own terms, never linked, embedded or redistributed
// by bashy. Do not label it "MIT" in an inventory. Download + exec, never
// bundled; the release's SHASUMS256.txt is the transit check (pin a digest in
// binmgr's pins.go for a supply-chain root).
package bun

import (
	"context"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion pins the release tag fetched when none is requested.
// Override with $BASHY_BUN_VERSION.
const DefaultVersion = "bun-v1.4.2"

// Spec is the release-asset description: bun-<os>-<arch>.zip holding
// bun-<os>-<arch>/bun[.exe].
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	if !strings.HasPrefix(version, "bun-v") {
		version = "bun-v" + strings.TrimPrefix(version, "v")
	}
	return binmgr.GitHubSpec{
		Name: "bun", Repo: "oven-sh/bun", Version: version,
		Member: "bun", AssetMatch: assetMatch,
	}
}

// assetMatch maps Go's goarch onto bun's tokens (amd64→x64, arm64→aarch64)
// and refuses the profile/baseline/musl variants.
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	if !strings.HasSuffix(n, ".zip") || strings.Contains(n, "profile") || strings.Contains(n, "baseline") || strings.Contains(n, "musl") {
		return false
	}
	arch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[goarch]
	return n == fmt.Sprintf("bun-%s-%s.zip", goos, arch)
}

// Ensure fetches (if needed) the bun binary and returns its cached path.
// Idempotent: a cache hit does no network I/O.
func Ensure(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("bun: resolve: %w", err)
	}
	return binmgr.Ensure(ctx, tool)
}
