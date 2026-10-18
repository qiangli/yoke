package registry

import (
	"context"

	"github.com/qiangli/yoke/external/gws"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// gws — the Google Workspace CLI (googleworkspace/cli, Apache-2.0), tier 6
// (cloud). Host-first like gcloud: until sha256 pins for the reviewed
// release land in pkg/binmgr/pins.go (network-verified hashes — out of
// story f25d7aa4437c's scope), an unset $GWS_VERSION uses the host's own
// gws and never TOFU-downloads; an explicit $GWS_VERSION opts into a
// managed GitHub-release download verified against the release sidecar.
// PreferHost also exempts the entry from the default-registry pin gate,
// which exists for silently-downloaded tools, not host-first ones.
//
// Gmail watch and Events subscribe/renew are deferred to the durable-event
// story, and Admin Directory CRUD is unclaimed until tested — see
// external/gws for the full admission record.
func init() {
	register(Entry{
		Name:       "gws",
		Tier:       6,
		License:    gws.License,
		Synopsis:   "Google Workspace CLI gws (managed external, Apache-2.0; host-first, pre-v1)",
		EnvVersion: "GWS_VERSION",
		PreferHost: true,
		Long: `gws (googleworkspace/cli, Apache-2.0) fronts Drive, Gmail, Calendar, Sheets,
Docs, Chat, Slides, Tasks and Contacts with explicit OAuth or service-account
credentials. $GWS_VERSION pins the release (reviewed: ` + gws.PinnedVersion + `);
unset uses the host's own gws. All args pass through to gws.

gws is pre-v1 and may break between releases, and it is NOT an officially
supported Google product. Gmail watch and Events subscribe/renew are deferred
to the durable-event story; Admin Directory CRUD is unclaimed until tested.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			if version == "" {
				version = gws.PinnedVersion
			}
			return binmgr.ResolveGitHub(ctx, gws.Spec(version))
		},
	})
}
