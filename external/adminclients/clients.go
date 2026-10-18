// Package adminclients is the Sprint 457 admission record (story
// 66494446f180) for the Microsoft 365 CLI (m365) and GAM (Google Workspace
// administration). Both are short-lived clients (supply-chain Rule 2);
// bulk/looping/interactive modes are separate and denied (modes.go). Typed
// errors, redaction, pagination and the loom-v2 envelope reuse external/gws
// (no new ABI), and GAM reuses gws's credential selection (scope.go) rather
// than keeping a second credential store.
package adminclients

import (
	"fmt"
	"sort"
)

// Client is the admission record of one administration CLI.
type Client struct {
	Name          string
	Repo          string // upstream source repository
	License       string // SPDX id of the CLIENT only
	Version       string // reviewed release tag
	Commit        string // immutable commit the tag points at
	InstallHint   string
	RemoteService string
	// Artifact is the npm tarball integrity (m365) — GAM uses Digests.
	Artifact string
	// Digests maps reviewed release asset names to GitHub's SHA-256 for that
	// exact asset (release API digest, recorded 2026-10-18).
	Digests map[string]string
}

var clients = map[string]Client{
	"m365": {
		Name: "m365", Repo: "pnp/cli-microsoft365", License: "MIT",
		Version: "v11.11.0", Commit: "628d601cdbe6c1d38ba86b36fc06520428a20fcb",
		InstallHint:   "install @pnp/cli-microsoft365@11.11.0 (npm i -g @pnp/cli-microsoft365@11.11.0, Node runtime required) and put m365 on PATH; the project publishes an npm package only, no binary archive",
		RemoteService: "Microsoft 365 / Entra ID / Microsoft Graph (proprietary hosted service under the tenant owner's agreement)",
		Artifact:      "npm @pnp/cli-microsoft365@11.11.0 shasum 78b944f232cc66618e7db7cce73b785cd6b5bbcc integrity sha512-GXdN3Cw4lOEFixVfI6YZlGM9OzAlMbjGItT8yMn8FznlG6+aWGNI6xjgNJRbigykeSE6YI3KRai+fijMt24DMg==",
	},
	"gam": {
		Name: "gam", Repo: "GAM-team/GAM", License: "Apache-2.0",
		Version: "v7.48.30", Commit: "e8a3bc8061b0c5633438121424b8994c3706543f",
		InstallHint:   "install GAM v7.48.30 from the GAM-team/GAM release (verify the SHA-256 in EVIDENCE.md) and put gam on PATH; releases are .tar.xz/.zip/.exe, which binmgr does not extract",
		RemoteService: "Google Workspace Admin SDK (proprietary hosted service under the domain owner's agreement)",
		Digests: map[string]string{
			"gam-7.48.30-linux-x86_64-glibc2.35.tar.xz": "3f15da84a2b49eb5abbc7e8cfbc5b1ebee8afe255678894bbf78e5f1ca2db125",
			"gam-7.48.30-linux-arm64-glibc2.35.tar.xz":  "3888ad86a9d109c5f5776908ae67bc3ff0b8953d11a0a6ed15e47fdeee68f375",
			"gam-7.48.30-macos15.7-arm64.tar.xz":        "027609235d1cd5a0b4798487765a00d978cd28407faffad7888e0dc41c28741e",
			"gam-7.48.30-macos15.7-x86_64.tar.xz":       "9d7bb70dffe5d24baac0c0205f6aa7e0d70581f0e759d1514da6555306a363d8",
			"gam-7.48.30-macos26.6-arm64.tar.xz":        "67fbad3d2343cddb5da7a8084c3f03b2c8d6786e8319ffdb6d692dbdefbd94cf",
			"gam-7.48.30-macos26.6-x86_64.tar.xz":       "86eabef9cde5194f160cc9d9599a7964f2f090a91f18ad02c554f1c7517ccd41",
			"gam-7.48.30-windows-x86_64.zip":            "78468701c525a9c3350ea48e33c2844715fe68416ac98807b2f4b9834ef6adaf",
			"gam-7.48.30-windows-arm64.zip":             "a8e7c145ba3b45424af6fa7e809e894189617eec51966280153cbeaa175d7243",
		},
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

// HostOnlyError is the registry Resolve result: neither client has an
// admitted binmgr archive, so both resolve from PATH only.
func HostOnlyError(name string) error {
	c, ok := Lookup(name)
	if !ok {
		return fmt.Errorf("%s: not an admitted client", name)
	}
	return fmt.Errorf("%s not found on PATH and no managed archive release is admitted — %s", name, c.InstallHint)
}
