package registry

import (
	"context"

	"github.com/qiangli/yoke/external/glab"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// glab — the GitLab CLI (gitlab-org/cli, MIT), tier 6 (cloud).
// Host-first like gcloud: glab publishes releases outside GitHub and no
// sha256 pin is committed, so the host's own glab is used and provisioning
// refuses with an actionable vendor-install hint instead of
// TOFU-downloading. PreferHost also exempts the entry from the
// default-registry pin gate, which exists for silently-downloaded tools,
// not host-first ones. See external/glab for the full admission record.
func init() {
	register(Entry{
		Name:       "glab",
		Tier:       6,
		License:    glab.License,
		Synopsis:   "GitLab CLI glab (managed external, MIT; host-first)",
		PreferHost: true,
		Long: `glab (gitlab-org/cli, MIT) fronts GitLab issues, merge requests and
pipelines with an explicit token (GITLAB_TOKEN or GITLAB_TOKEN_FILE).
bashy uses the host's own install when present; if absent it points at the
vendor install page rather than downloading. All args pass through to glab.
There is no ambient-account fallback, login never implies provisioning
permission, and glab has no dry-run: previews refuse without execution.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			return glab.Resolve(ctx, version)
		},
	})
}
