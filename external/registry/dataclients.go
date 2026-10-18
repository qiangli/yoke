package registry

import (
	"context"

	"github.com/qiangli/yoke/external/dataclients"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// Remote database and warehouse clients (story 9f087594f442), tier 6. Host-only
// short-lived clients to an explicitly configured endpoint: none has a
// sha256-pinned managed release, so Resolve is an actionable diagnostic and
// nothing is auto-downloaded. No server is installed or started, and the
// remote engine's licence is recorded separately in external/dataclients.
func init() {
	for _, d := range []struct{ name, synopsis string }{
		{"psql", "PostgreSQL client psql (host-only external, PostgreSQL licence; remote endpoint only)"},
		{"mongosh", "MongoDB Shell mongosh (host-only external, Apache-2.0; remote endpoint only)"},
		{"valkey-cli", "Valkey client valkey-cli (host-only external, BSD-3-Clause; remote endpoint only)"},
		{"snow", "Snowflake CLI snow (host-only external, Apache-2.0; remote warehouse only)"},
	} {
		name := d.name
		c, _ := dataclients.Lookup(name)
		register(Entry{
			Name:       name,
			Tier:       6,
			License:    c.License,
			Synopsis:   d.synopsis,
			PreferHost: true,
			Long: name + ` is a short-lived client to an explicitly configured, externally managed
endpoint (` + c.RemoteEngine + `). bashy never installs, starts or provisions a
server, and uses the host's own client (no managed download is admitted). All
args pass through; the typed bounded/rollback path is dataclients.Execute.
Client licence: ` + c.License + `. Remote engine licence (separate): ` + c.RemoteLicense + `.`,
			Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
				return binmgr.Tool{}, dataclients.HostOnlyError(name)
			},
		})
	}
}
