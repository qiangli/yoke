package binmgr

import (
	"context"
)

// OpenClawVersion is the exact reviewed npm release of the OpenClaw CLI used
// for feed messaging. It is pinned, never "latest": the tarball URL, the
// registry integrity (sha512) and the committed sha256 pin below must all
// agree before the bytes are installed.
//
// Reviewed 2026-10-14 against the npm registry:
//   - metadata (npm view + registry API independently agree):
//     tarball https://registry.npmjs.org/openclaw/-/openclaw-2026.10.2.tgz
//     shasum 27956be5ad49487209be231788fe6b89835185c9 (== sha1 of the bytes)
//     integrity sha512-FuLLF+WI12ypwQ0r+pcKDcmB+kXus5N4QClaKEnaAn6GN4I1AitkOhwv0GFl2rVKpvuUBDQWIfglHNy6Trv/EQ==
//     (== sha512 of the bytes; the registry also publishes SLSA provenance
//     attestations for this version)
//   - CLI contract read from the shipped docs/cli/message.md inside that exact
//     tarball (never a running service): `openclaw message send` takes
//     --channel/--target/--message plus --json, whose success shape carries a
//     top-level messageId. Rule 2: this package only installs the CLI; it
//     never starts the upstream gateway.
//
// The npm tarball is platform-independent (Node runs package/openclaw.mjs, a
// +x entry point preserved by Tree extraction), so every platform pins the
// same digest. Node >=24.16 <25 || >=26.1.0 runs it (package.json engines;
// feed doctor gates the same floor).
const (
	OpenClawName    = "openclaw"
	OpenClawVersion = "2026.10.2"

	// OpenClawTarball is the exact registry tarball for the pinned release.
	OpenClawTarball = "https://registry.npmjs.org/openclaw/-/openclaw-2026.10.2.tgz"
	// OpenClawSHA512 is the hex of the registry's integrity sha512 for that
	// tarball (the strongest vendor checksum; carried on the asset).
	OpenClawSHA512 = "16e2cb17e588d76ca9c10d2bfa970a0dc981fa45eeb3937840295a2849da027e86378235022b643a1c2fd06165dab54aa6fb9404341621f8251cdcba4ebbff11"
	// OpenClawSHA1 is the registry's shasum for that tarball (sha1 of the
	// downloaded bytes, corroborated at review time).
	OpenClawSHA1 = "27956be5ad49487209be231788fe6b89835185c9"
	// OpenClawSHA256 is the sha256 of the reviewed tarball bytes, committed
	// here via pins.go. At Ensure time the committed pin is authoritative and
	// overrides the asset digest (see pins.go for why the trust root is this
	// repo's history, not the release).
	OpenClawSHA256 = "d1c979c9ff6c5fb600a4582a7d777e0ef5264f5a570d2e822323a8c508d744fe"
)

// OpenClawPlatforms is every platform the pinned CLI installs on. The tarball
// is identical on all of them (same digest); Node executes the entry point.
var OpenClawPlatforms = []string{
	"linux/amd64", "linux/arm64",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
}

// OpenClawEntrypoint is the executable's path within the extracted npm tree.
const OpenClawEntrypoint = "package/openclaw.mjs"

// OpenClawTool declares the pinned npm tarball for the current platforms.
// Verification is the standard Ensure path: the committed pins.go sha256
// overrides the asset digest, so a tampered tarball is refused before
// anything is unpacked. (No Sigstore: npm tarballs carry no per-file
// Sigstore bundle that binmgr verifies; the registry integrity + committed
// pin is the integrity story.)
func OpenClawTool() Tool {
	t := Tool{Name: OpenClawName, Version: OpenClawVersion, Assets: make(map[string]Asset)}
	for _, p := range OpenClawPlatforms {
		t.Assets[p] = Asset{
			URL:        OpenClawTarball,
			SHA512:     OpenClawSHA512,
			Tree:       true,
			Entrypoint: OpenClawEntrypoint,
		}
	}
	return t
}

// OpenClawPath is where EnsureOpenClaw installs the CLI entry point:
// the openclaw version dir, at the npm tree entry point. It touches neither
// the network nor the filesystem. Errors exactly where the ensure would: no
// committed pin for this platform.
func OpenClawPath() (string, error) {
	return InstallPath(OpenClawTool())
}

// EnsureOpenClaw provisions the pinned OpenClaw CLI entry point into the
// openclaw version dir. Verification is the standard fail-closed Ensure path
// (committed pin, digest check before install) with the evidence cached for
// offline re-verification.
//
// Archive-only is NOT a runnable CLI: the tarball bundles 3 of the 65
// production dependencies and Windows cannot exec .mjs. Callers needing to
// run it use node.EnsureNpmTool (external/node: same verified archive plus
// the npm-installed production graph) and launch with node.NpmToolCommand;
// the feed adapter does this whenever the declaration sets runtime: node.
func EnsureOpenClaw(ctx context.Context) (string, error) {
	return Ensure(ctx, OpenClawTool())
}
