package registry

import (
	"context"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// TofuVersion is the default pinned release of OpenTofu.
const TofuVersion = "v1.13.1"

// tofu — OpenTofu (opentofu/opentofu, MPL-2.0), tier 6 (cloud): the
// infrastructure-as-code CLI, and the processor behind the Bash# `~~~tf`
// text fence (`iac.plan()`, `iac.apply()`). Ships per-platform GitHub-release
// archives (tofu_<ver>_<goos>_<goarch>.tar.gz / .zip) with the tofu binary at
// the archive root and a tofu_<ver>_SHA256SUMS list, so the default binmgr
// GitHub resolver handles it fail-closed. OpenTofu, not Terraform: the
// persistence-license policy admits MPL-2.0 and refuses the BSL.
func init() {
	register(Entry{
		Name:           "tofu",
		Tier:           6,
		License:        "MPL-2.0",
		Synopsis:       "OpenTofu infrastructure-as-code CLI (managed external, MPL-2.0)",
		EnvVersion:     "TOFU_VERSION",
		DefaultVersion: TofuVersion,
		Long: `tofu (opentofu/opentofu, MPL-2.0) is the OpenTofu infrastructure-as-code
CLI — downloaded from GitHub releases, sha256-verified against pinned digests,
and cached by binmgr (not compiled in). $TOFU_VERSION pins the release; all args
pass through to tofu. It is also the processor of the Bash# '~~~tf as iac' text
fence: iac.init(), iac.plan(), iac.apply(), iac.destroy().`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			if version == "" {
				version = TofuVersion
			}
			return binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
				Name:       "tofu",
				Repo:       "opentofu/opentofu",
				Version:    version,
				Member:     binmgr.BinaryName("tofu"),
				AssetMatch: tofuAssetMatch,
			})
		},
	})
}

func tofuAssetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	if !strings.HasSuffix(n, ext) {
		return false
	}
	if !strings.Contains(n, "_"+goos+"_") {
		return false
	}
	return strings.Contains(n, "_"+goarch+".") || strings.HasSuffix(n, "_"+goarch+ext)
}
