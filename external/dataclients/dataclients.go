// Package dataclients admits four remote database and warehouse command
// clients — psql, mongosh, valkey-cli and snow — as short-lived clients to
// an explicitly configured, externally managed endpoint (Sprint 457 story
// 9f087594f442, client admission contract mode "external user-managed
// endpoint").
//
// What this is not: no server is installed, started, restarted or
// provisioned, and none of these clients satisfies the Sprint 453 embedded
// in-memory/persistent engine, shared-owner or exclusive-claim contracts.
// A client licence is recorded here; the licence of whatever engine answers
// at the endpoint is a separate fact (RemoteEngine / RemoteLicense) and is
// never inherited from, or granted by, the client.
//
// Provisioning is host-first (registry PreferHost): none of the four ships
// a pinned, hash-verified standalone release in pkg/binmgr/pins.go, so an
// absent host client is an actionable diagnostic, never an auto-download.
//
// The typed operation (Build/Run) is the guarded path: explicit endpoint,
// TLS required off-loopback, credentials only via env/file (never argv),
// statements on stdin, bounded reads, context cancellation, and a
// transaction that rolls back unless the caller explicitly commits. The
// `bashy <client>` registry front-door stays a raw passthrough, like gws.
// Errors reuse external/gws typed codes over the loom-v2 envelope; no
// competing error ABI or schema is defined here.
package dataclients

import (
	"fmt"
	"sort"
)

// Client is the admission record of one remote data client.
type Client struct {
	Name          string // verb and binary basename
	Prefix        string // env prefix for explicit endpoint/credential selection
	License       string // SPDX id of the CLIENT only
	RemoteEngine  string // what usually answers at the endpoint
	RemoteLicense string // licence of that engine/service — separate from the client
	DefaultPort   int
	Fidelity      string // declared output representation (lossy ones say so)
	InstallHint   string
}

var clients = map[string]Client{
	"psql": {
		Name: "psql", Prefix: "PSQL", License: "PostgreSQL", DefaultPort: 5432,
		RemoteEngine: "PostgreSQL server (or wire-compatible)", RemoteLicense: "PostgreSQL (permissive); wire-compatible forks and managed services carry their own terms",
		Fidelity:    "text: non-null values are text as rendered by the server (no precision loss); SQL NULL is JSON null",
		InstallHint: "install the PostgreSQL client package (postgresql-client / libpq) and put psql on PATH",
	},
	"mongosh": {
		Name: "mongosh", Prefix: "MONGOSH", License: "Apache-2.0", DefaultPort: 27017,
		RemoteEngine: "MongoDB server (or wire-compatible)", RemoteLicense: "SSPL-1.0 for MongoDB Community Server (not OSI-permissive); Atlas/managed and compatible services carry their own terms",
		Fidelity:    "canonical Extended JSON (EJSON relaxed=false): int/long/decimal/date/binary/ObjectId types preserved",
		InstallHint: "install mongosh from https://www.mongodb.com/try/download/shell and put it on PATH",
	},
	"valkey-cli": {
		Name: "valkey-cli", Prefix: "VALKEY", License: "BSD-3-Clause", DefaultPort: 6379,
		RemoteEngine: "Valkey server, or a Redis-protocol-compatible server", RemoteLicense: "BSD-3-Clause for Valkey; Redis 7.4+ is RSALv2/SSPLv1 (8.0+ also AGPLv3) — recorded separately, not admitted by this client",
		Fidelity:    "quoted JSON: integers numeric, nil is null, binary-safe strings escaped",
		InstallHint: "install the Valkey client tools (valkey-tools / valkey) and put valkey-cli on PATH",
	},
	"snow": {
		Name: "snow", Prefix: "SNOW", License: "Apache-2.0", DefaultPort: 443,
		RemoteEngine: "Snowflake cloud data warehouse (SaaS)", RemoteLicense: "proprietary hosted service under the account holder's own Snowflake agreement; not an open-source engine",
		Fidelity:    "provider JSON: Snowflake renders NUMBER/TIMESTAMP/BINARY as strings or numbers per its own JSON result format; no further coercion",
		InstallHint: "install Snowflake CLI (pipx install snowflake-cli) and put snow on PATH",
	},
}

// Lookup returns the admission record for a client name.
func Lookup(name string) (Client, bool) { c, ok := clients[name]; return c, ok }

// Names lists the admitted client names, sorted.
func Names() []string {
	out := make([]string, 0, len(clients))
	for n := range clients {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HostOnlyError is the registry Resolve result: no managed download is
// admitted, so an absent host client yields an actionable diagnostic.
func HostOnlyError(name string) error {
	c, ok := Lookup(name)
	if !ok {
		return fmt.Errorf("%s: not an admitted data client", name)
	}
	return fmt.Errorf("%s not found on PATH and no sha256-pinned managed release is admitted — %s", name, c.InstallHint)
}
