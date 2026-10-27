package loom

import (
	"net"
	"os"
	"strings"
)

// MirrorAllowedDomainsEnv opts this loom into push-mirroring to replicas on
// private or loopback addresses — the selvedge case
// (docs/selvedge-self-hosted-git.md): every mesh-local replica is a private
// address, and Gitea refuses a push mirror to one unless [migrations]
// ALLOW_LOCALNETWORKS is set, answering 401 "Permission denied" (its
// HandleRemoteAddressError maps ErrInvalidCloneAddr{IsPermissionDenied} to 401,
// which reads like an auth failure and is not one).
//
// The value is a comma list of replica host[:port] patterns. It is NOT merely
// documentation: enabling local networks wholesale is an SSRF hole on a hosted
// multi-owner satellite, whose own services listen on loopback, so the opt-in
// is per-host and explicit, and the domain list narrows what the loosened guard
// can reach. When the env is unset loom writes nothing and Gitea's defaults
// stand.
const MirrorAllowedDomainsEnv = "LOOM_MIRROR_ALLOWED_DOMAINS"

// githubMirrorDomains keep GitHub reachable once ALLOWED_DOMAINS is set. This is
// load-bearing, not decoration: Gitea's migrations Init appends its
// MatchBuiltinExternal allow-rule ONLY while ALLOWED_DOMAINS is empty, so the
// moment loom names a replica host every other external host — github.com, the
// S515.5 GitHub push mirror's target — stops being allowed.
var githubMirrorDomains = []string{"github.com", "*.github.com"}

// mirrorAllowedDomains reads MirrorAllowedDomainsEnv.
func mirrorAllowedDomains() string {
	return migrationsAllowedDomains(os.Getenv(MirrorAllowedDomainsEnv))
}

// migrationsAllowedDomains renders the [migrations] ALLOWED_DOMAINS value for a
// raw env setting: the operator's replica patterns, then the GitHub pair, with
// no duplicates. It returns "" for an unset (or blank) setting — the signal to
// leave app.ini alone.
//
// Ports are stripped. Gitea matches this list against the remote address's
// HOSTNAME (IsMigrateURLAllowed splits the port off before matching), so a
// pattern carrying one could never match; accepting host[:port] and normalizing
// it keeps the env in the shape an operator reads off a replica URL.
func migrationsAllowedDomains(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var out []string
	seen := map[string]bool{}
	add := func(pattern string) {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return
		}
		pattern = stripPort(pattern)
		if key := strings.ToLower(pattern); pattern != "" && !seen[key] {
			seen[key] = true
			out = append(out, pattern)
		}
	}
	for _, pattern := range strings.Split(raw, ",") {
		add(pattern)
	}
	for _, pattern := range githubMirrorDomains {
		add(pattern)
	}
	return strings.Join(out, ",")
}

// stripPort drops a :port suffix, leaving a bare IPv6 literal ("::1") and a
// wildcard pattern ("*.github.com") untouched.
func stripPort(pattern string) string {
	if host, _, err := net.SplitHostPort(pattern); err == nil {
		return host
	}
	return pattern
}

// ensureMigrationsSection records the mirror allow-list in app.ini. An empty
// domains argument writes nothing: a loom that never opted in keeps Gitea's
// defaults, and one that opted in earlier keeps the keys it already has (this
// converges app.ini forward, it does not police it).
func ensureMigrationsSection(ini, domains string) string {
	if domains == "" {
		return ini
	}
	return ensureSectionKeys(ini, "migrations", map[string]string{
		"ALLOW_LOCALNETWORKS": "true",
		"ALLOWED_DOMAINS":     domains,
	})
}
