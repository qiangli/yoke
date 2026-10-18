package registry

import (
	"context"

	"github.com/qiangli/yoke/external/aws"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// aws — the Amazon Web Services CLI v2 (Apache-2.0), tier 6 (cloud).
// Host-first like gcloud: the CLI is a large vendor distribution whose
// installer is the trusted, self-updating path, so the host's own aws is
// used and provisioning refuses with an actionable vendor-install hint
// instead of TOFU-downloading. PreferHost also exempts the entry from the
// default-registry pin gate, which exists for silently-downloaded tools,
// not host-first ones. See external/aws for the full admission record.
func init() {
	register(Entry{
		Name:       "aws",
		Tier:       6,
		License:    aws.License,
		Synopsis:   "AWS CLI v2 (managed external, Apache-2.0; host-first)",
		PreferHost: true,
		Long: `aws (Amazon Web Services CLI v2, Apache-2.0) fronts EC2, S3 and IAM
with an explicit profile (AWS_PROFILE) plus a credential source. bashy
uses the host's own install when present; if absent it points at the vendor
installer rather than downloading. All args pass through to aws. There is
no ambient-profile fallback and login never implies provisioning
permission. EC2/S3 mutations preview with --dry-run/--dryrun; other dry
runs refuse without execution.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			return aws.Resolve(ctx, version)
		},
	})
}
