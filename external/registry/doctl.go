package registry

import (
	"context"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// doctl — the DigitalOcean CLI (digitalocean/doctl, Apache-2.0), tier 6 (cloud).
// Ships per-platform GitHub-release archives (doctl-<ver>-<goos>-<goarch>.tar.gz
// / .zip) with the doctl binary at the archive root and a *-checksums.sha256
// file, so the default binmgr GitHub resolver handles it. First tier-6 verb;
// the daily-driver cloud CLI.
func init() {
	register(Entry{
		Name:       "doctl",
		Tier:       6,
		License:    "Apache-2.0",
		Synopsis:   "DigitalOcean CLI (managed external, Apache-2.0)",
		EnvVersion: "DOCTL_VERSION",
		Long: `doctl (digitalocean/doctl, Apache-2.0) is the DigitalOcean cloud CLI —
downloaded from GitHub releases, sha256-verified, and cached by binmgr (not
compiled in). $DOCTL_VERSION pins the release; all args pass through to doctl.
Authenticate with 'bashy doctl auth init' (token via 'bashy secrets').`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			return binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
				Name:    "doctl",
				Repo:    "digitalocean/doctl",
				Version: version, // "" → latest
				Member:  binmgr.BinaryName("doctl"),
			})
		},
	})
}
