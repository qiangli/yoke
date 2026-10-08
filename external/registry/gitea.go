package registry

import (
	"context"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// GiteaVersion is the default pinned release of Gitea.
const GiteaVersion = "v1.27.3"

// gitea — the lightweight Git forge (go-gitea/gitea, MIT), tier 5 (cluster).
// Ships per-platform GitHub-release raw binaries and checksums.
func init() {
	register(Entry{
		Name:           "gitea",
		Tier:           5,
		License:        "MIT",
		Synopsis:       "Gitea lightweight Git forge (managed external, MIT)",
		EnvVersion:     "GITEA_VERSION",
		DefaultVersion: GiteaVersion,
		Long: `gitea (go-gitea/gitea, MIT) is the lightweight Git forge — downloaded from
GitHub releases, sha256-verified against pinned digests, and cached by binmgr (not
compiled in). $GITEA_VERSION pins the release; all args pass through to gitea.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			if version == "" {
				version = GiteaVersion
			}
			return binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
				Name:    "gitea",
				Repo:    "go-gitea/gitea",
				Version: version,
			})
		},
	})
}
