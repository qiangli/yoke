package registry

import (
	"context"
	"os"

	"github.com/qiangli/yoke/external/adminclients"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// Microsoft 365 (m365) and GAM Google Workspace administration clients (story
// 66494446f180), tier 6, short-lived clients only, host-only: neither
// publishes an archive binmgr can verify and extract (m365 is npm-only; GAM
// ships .tar.xz), so no download is ever attempted. Bulk, raw-request,
// interactive-login and gws-overlapping invocations are refused — see
// adminclients.ClassifyArgs/AdmitMode.
func init() {
	for _, d := range []struct{ name, synopsis string }{
		{"m365", "Microsoft 365 CLI (host-only external, MIT; explicit test tenant, least-scope Graph permissions)"},
		{"gam", "GAM Workspace admin (host-only external, Apache-2.0; admin gaps gws defers only, reuses the gws credential selection)"},
	} {
		name := d.name
		c, _ := adminclients.Lookup(name)
		register(Entry{
			Name:       name,
			Tier:       6,
			License:    c.License,
			Synopsis:   d.synopsis,
			PreferHost: true,
			Long: name + ` (` + c.Repo + `, ` + c.License + `; reviewed ` + c.Version + `) is a short-lived client with
explicit tenant/domain and credential selection (no ambient account). Bulk
fan-out, raw API requests and interactive logins are not admitted. All args
pass through. Remote service (separate): ` + c.RemoteService + `.`,
			ExecEnv: func() []string { return os.Environ() },
			Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
				return binmgr.Tool{}, adminclients.HostOnlyError(name)
			},
		})
	}
}
