package registry

import (
	"context"

	"github.com/qiangli/yoke/external/gcloud"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// gcloud — the Google Cloud SDK CLI (Apache-2.0), tier 6 (cloud). Unlike doctl
// (a single-binary GitHub release the binmgr GitHub resolver handles), gcloud is
// the Python Cloud SDK that self-updates via its own installer, so bashy prefers
// the host's install (PreferHost) and points at the vendor installer when absent
// — the provisioning logic lives in external/gcloud (a spin-out candidate for
// qiangli/gcloud). All args pass through.
//
// Workspace note: `gcloud auth print-access-token` yields an OAuth token for the
// Google REST APIs (Gmail/Calendar/Drive), but that access belongs in kg / the
// agentic layer, NOT a bashy core verb — see docs/email-workspace-access-strategy.md.
func init() {
	register(Entry{
		Name:       "gcloud",
		Tier:       6,
		License:    gcloud.License,
		Synopsis:   "Google Cloud SDK CLI (managed external, Apache-2.0; host-first)",
		PreferHost: true,
		Long: `gcloud (Google Cloud SDK, Apache-2.0) is Google's cloud CLI. bashy uses the
host's own install when present — the vendor installer + 'gcloud components
update' is the trusted, self-updating path; if absent it points at the official
installer rather than downloading the SDK. All args pass through to gcloud.
Authenticate with 'bashy gcloud auth login'.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			return gcloud.Resolve(ctx, version)
		},
	})
}
