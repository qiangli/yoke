package registry

import (
	"context"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// tofu — OpenTofu (opentofu/opentofu, MPL-2.0), tier 6 (cloud): the
// infrastructure-as-code CLI, and the processor behind the Bash# `~~~tf`
// text fence (`iac.plan()`, `iac.apply()`). Ships per-platform GitHub-release
// archives (tofu_<ver>_<goos>_<goarch>.tar.gz / .zip) with the tofu binary at
// the archive root and a tofu_<ver>_SHA256SUMS list, so the default binmgr
// GitHub resolver handles it fail-closed. OpenTofu, not Terraform: the
// persistence-license policy admits MPL-2.0 and refuses the BSL.
func init() {
	register(Entry{
		Name:       "tofu",
		Tier:       6,
		License:    "MPL-2.0",
		Synopsis:   "OpenTofu infrastructure-as-code CLI (managed external, MPL-2.0)",
		EnvVersion: "TOFU_VERSION",
		Long: `tofu (opentofu/opentofu, MPL-2.0) is the OpenTofu infrastructure-as-code
CLI — downloaded from GitHub releases, sha256-verified against the release's
SHA256SUMS, and cached by binmgr (not compiled in). $TOFU_VERSION pins the
release; all args pass through to tofu. It is also the processor of the
Bash# '~~~tf as iac' text fence: iac.init(), iac.plan(), iac.apply(),
iac.destroy().`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			return binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
				Name:    "tofu",
				Repo:    "opentofu/opentofu",
				Version: version, // "" → latest
				Member:  binmgr.BinaryName("tofu"),
			})
		},
	})
}
