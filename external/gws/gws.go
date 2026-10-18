// Package gws fronts the Google Workspace CLI (googleworkspace/cli, "gws")
// as a managed external binary (pkg/binmgr): resolved → verified → cached,
// never compiled in. `bashy gws …` is a transparent passthrough — it ensures
// the gws binary, then execs it with your args.
//
// Admission (Sprint 457 story f25d7aa4437c): gws is Apache-2.0, pinned to the
// reviewed release PinnedVersion (immutable commit PinnedCommit). It is a
// short-lived client under the service supply-chain policy (Rule 2): one
// bounded invocation, exits on completion, no listener or resident child.
// Gmail watch and Events subscribe/renew are NOT implemented here — the
// durable-event story 5548854717bc provides them as opt-in adapters in
// external/gws/events. Admin Directory CRUD is unclaimed until tested.
//
// Two notices every consumer must keep:
//
//   - gws is pre-v1 (v0.x): upstream may make breaking CLI/API changes
//     between releases. Upgrades are deliberate reviewed tag bumps only.
//   - gws is NOT an officially supported Google product; it is a
//     community-maintained CLI over Google's public Workspace APIs and
//     runtime Discovery documents. A binary pin does not freeze the remote
//     API: Discovery schema versions and cache fingerprints are recorded
//     separately (see EVIDENCE.md) and drift stays a named gap.
//
// Provisioning posture: host-first. Until sha256 pins for the reviewed
// release land in pkg/binmgr/pins.go (needs network-verified hashes —
// explicitly out of this story's scope), the registry entry prefers a host
// gws and refuses silent auto-provisioning; an explicit GWS_VERSION opts
// into a managed GitHub-release download whose checksum is verified against
// the release's own sidecar (TOFU — recorded as a gap in EVIDENCE.md).
// Live Google verification needs an authorized test account, which this
// environment does not have; that stays a named unverified gap, never
// inferred support.
package gws

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/notrack"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// Admission facts. Release v0.22.5, reviewed at the immutable commit below;
// the service-registry source hash covers the reviewed source tree's
// service list, not an executable.
const (
	// License is the SPDX id of googleworkspace/cli. Recorded for admission;
	// download+exec ≠ bundle, so nothing propagates.
	License = "Apache-2.0"
	// Repo is the upstream source repository.
	Repo = "googleworkspace/cli"
	// PinnedVersion is the reviewed release tag. Upgrades are deliberate
	// reviewed tag bumps, never "latest".
	PinnedVersion = "v0.22.5"
	// PinnedCommit is the immutable commit the reviewed tag points at.
	PinnedCommit = "a3768d0e82ad83cca2da97724e46bea4ff0e6dbd"
	// ReleaseURL is the human-checkable release page.
	ReleaseURL = "https://github.com/googleworkspace/cli/releases/tag/v0.22.5"
)

// Spec is the binmgr GitHub spec. gws ships per-platform archives named
// google-workspace-cli-<arch>-<os>.tar.gz (.zip on Windows) with the gws
// binary at the archive root (matched by basename; the Windows `.exe` is
// handled by binmgr). The arch/os tokens are not Go-standard, hence the
// custom AssetMatch. Checksums come from the release's .sha256 sidecars.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = PinnedVersion
	}
	return binmgr.GitHubSpec{
		Name: "gws", Repo: Repo, Version: version,
		Member: "gws", AssetMatch: assetMatch,
	}
}

// assetMatch maps Go's goos/goarch onto gws's release tokens
// (x86_64/aarch64; apple-darwin / unknown-linux-gnu / pc-windows-msvc).
// Only the gnu Linux archive is matched: the musl variant is an unverified
// alternative, and matching both would make resolution ambiguous.
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	var archTok string
	switch goarch {
	case "amd64":
		archTok = "x86_64"
	case "arm64":
		archTok = "aarch64"
	default:
		return false
	}
	var osTok string
	switch goos {
	case "darwin":
		osTok = "apple-darwin"
	case "linux":
		osTok = "unknown-linux-gnu"
	case "windows":
		osTok = "pc-windows-msvc"
	default:
		return false
	}
	return strings.Contains(n, osTok) && strings.Contains(n, archTok)
}

// Ensure resolves (if needed) the gws binary and returns its cached path.
// An empty version resolves the pinned release.
func Ensure(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("gws: resolve: %w", err)
	}
	return binmgr.Ensure(ctx, tool)
}

// Path resolves + caches the gws binary at the pinned release.
func Path(ctx context.Context) (string, error) {
	return Ensure(ctx, PinnedVersion)
}

// Run execs the cached gws with the given args, inheriting stdio — a
// transparent passthrough so `bashy gws <service> …` works. Returns the
// child's exit code via the returned error (an executed foreign status
// passes through; it is never rewritten into a native exit).
func Run(ctx context.Context, args []string) error {
	bin, err := Path(ctx)
	if err != nil {
		return err
	}
	c := binmgr.Command(ctx, bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = notrack.Env(os.Environ())
	return c.Run()
}

// ChildEnv returns env with gws tracking/update checks disabled.
func ChildEnv(env []string) []string { return notrack.Env(env) }

// NewGwsCmd builds the `gws` passthrough command (bashy front-door).
// $GWS_VERSION pins the release; all args pass through to gws. Credential
// selection stays explicit (see auth.go): this command never falls back to
// an ambient account.
func NewGwsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gws",
		Short: "Run the Google Workspace CLI gws (managed external binary, Apache-2.0)",
		Long: `gws runs the Google Workspace CLI (googleworkspace/cli, Apache-2.0) — resolved,
verified, and cached by binmgr (not compiled in). All args pass through to gws
($GWS_VERSION pins the release; default ` + PinnedVersion + `).

gws is pre-v1 and may break between releases, and it is NOT an officially
supported Google product. Authenticate explicitly per invocation (see the
credentials story); there is no ambient-account fallback. Gmail watch and
Events subscribe/renew go through the opt-in durable adapters in
external/gws/events (offline-verified; live renewal unverified), and Admin
Directory CRUD is unclaimed until tested.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), args)
		},
	}
}
