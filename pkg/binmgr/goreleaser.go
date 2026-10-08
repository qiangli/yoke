// Package binmgr's goreleaser tool: the pinned release-engineering binary that
// `bashy release` delegates to for whatever its embedded stages do not cover.
//
// The binary is never user-facing — there is no `bashy goreleaser` command —
// so this file names the fork, the pinned version and the trust mechanism, and
// nothing else carries the word. Upstream project of record is
// goreleaser/goreleaser (MIT); the fork's README carries the credit.
package binmgr

import (
	"context"
	"fmt"
)

// GoreleaserName is the logical tool name: the binmgr cache key and the
// cached binary basename. It is internal to `bashy release`.
const GoreleaserName = "goreleaser"

// GoreleaserRepo is the fork that publishes the pinned bytes.
const GoreleaserRepo = "qiangli/goreleaser"

// GoreleaserVersion is the pinned fork release. The tag moves only by a
// reviewed change here plus new pins.go rows — never by tracking latest.
const GoreleaserVersion = "v2.18.2-bashy.1"

// GoreleaserPlatforms lists every platform the fork release must ship and
// pins.go must cover. A platform missing from either is a provisioning hole
// on that host, so the coverage test below fails closed on this list.
var GoreleaserPlatforms = []string{
	"darwin/amd64",
	"darwin/arm64",
	"linux/amd64",
	"linux/arm64",
	"windows/amd64",
	"windows/arm64",
}

// GoreleaserSpec locates the pinned binary on the fork's releases: raw
// per-platform binaries (no Member — the asset is itself the executable)
// plus a checksums.txt the resolver can read. The committed pins.go digest,
// not the release's checksums.txt, is the trust root (Ensure enforces it).
func GoreleaserSpec() GitHubSpec {
	return GitHubSpec{
		Name:    GoreleaserName,
		Repo:    GoreleaserRepo,
		Version: GoreleaserVersion,
	}
}

// EnsureGoreleaser resolves the pinned binary for this platform and returns
// its executable path, downloading + verifying + caching on first use.
// Fail-closed: without a committed pin for this platform there is no
// download, so an incompletely pinned release can never silently provision
// an unverified binary.
func EnsureGoreleaser(ctx context.Context) (string, error) {
	if _, ok := PinnedSHA256(GoreleaserName, GoreleaserVersion, Platform()); !ok {
		return "", fmt.Errorf("binmgr: %s %s has no committed sha256 for %s; refusing to provision unverified bytes", GoreleaserName, GoreleaserVersion, Platform())
	}
	tool, err := ResolveGitHub(ctx, GoreleaserSpec())
	if err != nil {
		return "", err
	}
	return Ensure(ctx, tool)
}
