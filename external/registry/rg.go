package registry

import (
	"context"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// RgVersion is the default pinned release of ripgrep.
const RgVersion = "15.1.0"

// rg - ripgrep (BurntSushi/ripgrep, MIT OR Unlicense), tier 2 userland.
// Ships per-platform release archives with the rg binary nested under the
// archive root directory. Checksums are resolved by binmgr from the release's
// checksum list.
func init() {
	register(Entry{
		Name:           "rg",
		Tier:           2,
		License:        "MIT OR Unlicense",
		Synopsis:       "ripgrep fast recursive text search (managed external, MIT OR Unlicense)",
		EnvVersion:     "RG_VERSION",
		DefaultVersion: RgVersion,
		Long: `rg runs ripgrep (BurntSushi/ripgrep) - downloaded from GitHub
releases, sha256-verified against pinned digests, and cached by binmgr (not compiled in).
$RG_VERSION pins the release; all args pass through to rg.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			if version == "" {
				version = RgVersion
			}
			return binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
				Name:       "rg",
				Repo:       "BurntSushi/ripgrep",
				Version:    version,
				Member:     "rg",
				AssetMatch: rgAssetMatch,
			})
		},
	})
}

func rgAssetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	if !strings.HasPrefix(n, "ripgrep-") {
		return false
	}
	if goos == "darwin" {
		return strings.Contains(n, "apple-darwin") && rgArchMatch(n, goarch)
	}
	if goos == "windows" {
		return strings.Contains(n, "pc-windows-msvc") && rgArchMatch(n, goarch)
	}
	if goos == "linux" {
		return strings.Contains(n, "unknown-linux") && rgArchMatch(n, goarch)
	}
	return strings.Contains(n, goos) && rgArchMatch(n, goarch)
}

func rgArchMatch(assetName, goarch string) bool {
	switch goarch {
	case "amd64":
		return strings.Contains(assetName, "x86_64")
	case "arm64":
		return strings.Contains(assetName, "aarch64")
	case "386":
		return strings.Contains(assetName, "i686")
	default:
		return strings.Contains(assetName, goarch)
	}
}
