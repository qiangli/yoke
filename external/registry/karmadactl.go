package registry

import (
	"context"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// KarmadactlVersion is the default pinned release of karmadactl.
const KarmadactlVersion = "v1.19.0"

// karmadactl — the Karmada CLI (karmada-io/karmada, Apache-2.0), tier 5
// (cluster). Ships per-platform release archives
// (karmadactl-<os>-<arch>.tgz, the karmadactl binary at the archive root)
// with a per-asset .tgz.sha256 sidecar for darwin/linux x amd64/arm64 —
// upstream publishes no Windows asset, so Windows stays fail-closed (no pin,
// no platform). Download + exec only, never bundled or vendored.
func init() {
	register(Entry{
		Name:           "karmadactl",
		Tier:           5,
		License:        "Apache-2.0",
		Synopsis:       "Karmada federation CLI (managed external, Apache-2.0)",
		EnvVersion:     "KARMADACTL_VERSION",
		DefaultVersion: KarmadactlVersion,
		Long: `karmadactl (karmada-io/karmada, Apache-2.0) is the Karmada
multi-cluster federation CLI — downloaded from GitHub releases,
sha256-verified against pinned digests, and cached by binmgr (not compiled
in). $KARMADACTL_VERSION pins the release; all args pass through to
karmadactl. Reached through 'bashy dks federation karmadactl ...', which
selects DKS_PROFILE=federation for its kubectl calls.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			if version == "" {
				version = KarmadactlVersion
			}
			return binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
				Name:       "karmadactl",
				Repo:       "karmada-io/karmada",
				Version:    version,
				Member:     binmgr.BinaryName("karmadactl"),
				AssetMatch: karmadactlAssetMatch,
			})
		},
	})
}

// karmadactlAssetMatch picks the karmadactl-<os>-<arch>.tgz archive for the
// current platform. The exact-name match keeps the sibling
// kubectl-karmada-<os>-<arch>.tgz plugin (shipped in the same release) from
// ever resolving as karmadactl.
func karmadactlAssetMatch(name, goos, goarch string) bool {
	want := "karmadactl-" + goos + "-" + goarch + ".tgz"
	return strings.ToLower(name) == want
}
