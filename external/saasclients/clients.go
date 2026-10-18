// Package saasclients is the Sprint 457 admission record (story c512b342864d)
// for the Salesforce (sf), Stripe (stripe), Twilio (twilio) and Slack (slack)
// CLIs. All four are short-lived clients (supply-chain Rule 2); webhook
// forwarding/serving and other resident modes are separate, denied-by-default
// modes (modes.go). Typed errors and redaction reuse external/gws (no new
// error ABI); results travel in the shared loom-v2 envelope.
package saasclients

import (
	"fmt"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

type binmgrSpec = binmgr.GitHubSpec

// Client is the admission record of one SaaS CLI.
type Client struct {
	Name          string // verb and binary basename
	Repo          string // upstream source repository
	License       string // SPDX id of the CLIENT only
	Version       string // reviewed release tag
	Commit        string // immutable commit the tag points at
	Managed       bool   // a binmgr-resolvable archive release is admitted
	InstallHint   string
	RemoteService string // the proprietary hosted service; separate from the client licence
	// Digests maps reviewed release asset names to GitHub's SHA-256 for that
	// exact asset (release API digest, recorded 2026-10-18).
	Digests map[string]string
}

var clients = map[string]Client{
	"sf": {
		Name: "sf", Repo: "salesforcecli/cli", License: "Apache-2.0",
		Version: "2.156.4", Commit: "54d34873cb87d1c0093b9f4dbd6c9756ff300e4b",
		InstallHint:   "install the Salesforce CLI release 2.156.4 (npm i -g @salesforce/cli@2.156.4 or the vendor tarball) and put sf on PATH",
		RemoteService: "Salesforce platform (proprietary hosted service under the org owner's agreement)",
		Digests: map[string]string{
			"sf-v2.156.4-54d3487-darwin-arm64.tar.gz": "11521e02ad76a74ffa19497d4d7af293d3bf4081aa581bffa2cd2f0c3bc80e18",
			"sf-v2.156.4-54d3487-darwin-x64.tar.gz":   "15dcbec511735a1cfacefa92b395d0bb4a761927836cdcb4724b56a80621670e",
			"sf-v2.156.4-54d3487-linux-arm64.tar.gz":  "8f00751a393b27325858cfb1bc710e90f8674949fcbe8e12544d8e44c7b5b401",
			"sf-v2.156.4-54d3487-linux-x64.tar.gz":    "2ffbee9be75b493b34f58fe96535dc1f8dcd284d7ca8fbcdf1cd404ae157ff5d",
			"sf-v2.156.4-54d3487-win32-x64.tar.gz":    "2942021d667d93a02e5d75ba3be54986d315ff31763d30128acbb1a1d5177fc5",
		},
	},
	"stripe": {
		Name: "stripe", Repo: "stripe/stripe-cli", License: "Apache-2.0",
		Version: "v1.54.0", Commit: "7839c312a40ed89ecc48d16fc5fb8bca7dbfac49", Managed: true,
		InstallHint:   "install stripe-cli v1.54.0 and put stripe on PATH",
		RemoteService: "Stripe API (proprietary hosted service under the account holder's agreement)",
		Digests: map[string]string{
			"stripe_1.54.0_mac-os_arm64.tar.gz":  "3595a68591ed694bd0a183a6ba94aeb79c00f8e2062649065674baa450c1caee",
			"stripe_1.54.0_mac-os_x86_64.tar.gz": "f7096210c203a0a28a35735f0b4cb047ebe842c3fba21a4aff2967f1e6e3c2ad",
			"stripe_1.54.0_linux_arm64.tar.gz":   "1a5fd5a7609ba71bed2bbe20115954b25cf2d36b690b18331e790f4e4882a72a",
			"stripe_1.54.0_linux_x86_64.tar.gz":  "7e8b0693b56b3690bf0bd5d50f1f8a96bd8b84ad4efd1f08c493220b191e074b",
			"stripe_1.54.0_windows_x86_64.zip":   "9653307aa5bf56f1fa8b8c1ca981e42b0be39af5d6ef5e5eb3686061450c7e4b",
		},
	},
	"twilio": {
		Name: "twilio", Repo: "twilio/twilio-cli", License: "MIT",
		Version: "7.2.0", Commit: "9c88e3b8e4ef44b5e9f1f11c967cfce8ccea8ca8",
		InstallHint:   "install twilio-cli 7.2.0 (npm i -g twilio-cli@7.2.0 or the vendor installer) and put twilio on PATH; the release publishes installers only, no archive",
		RemoteService: "Twilio API (proprietary hosted service under the account holder's agreement)",
		Digests: map[string]string{
			"twilio-7.2.0-x64.pkg": "e48c5bbb7e07ec1ecc130b4e0bdb886a28d63f8e210446d07d6db9b10eb9b985",
			"twilio-7.2.0.exe":     "4a28cda28fbc0bd35cdb1b6ece328d1361d75f6d0384e421f6733efe4217b47a",
		},
	},
	"slack": {
		Name: "slack", Repo: "slackapi/slack-cli", License: "Apache-2.0",
		Version: "v4.10.0", Commit: "e7ff104a324c4914e492eebaa7ceb34705a10460", Managed: true,
		InstallHint:   "install Slack CLI v4.10.0 and put slack on PATH",
		RemoteService: "Slack platform (proprietary hosted service under the workspace owner's agreement)",
		Digests: map[string]string{
			"slack_cli_4.10.0_macOS_arm64.tar.gz": "0f897eee17ce549ba1f34c389c2591c32c191ce94eea15cd4b92053d8f35c80a",
			"slack_cli_4.10.0_macOS_amd64.tar.gz": "1ae1db7c4da5e861e24ee4c0f09965829d0f7508a080f87692193678549b5895",
			"slack_cli_4.10.0_linux_arm64.tar.gz": "a930e089977e1138320efbe3bc49dc18f40d187bbbf4a9c7ff419c443bc52d30",
			"slack_cli_4.10.0_linux_amd64.tar.gz": "9772b17f70c4ec522ea6ae25545220dfe17d6afb5babe28fa1cd55a24b2abffa",
			"slack_cli_4.10.0_windows_64-bit.zip": "0c701123a7dc210a817737e48ac1bd17d1435770d279aa94bc4bff9d7dfc4423",
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

// HostOnlyError is the registry Resolve result for a client without an
// admitted managed archive.
func HostOnlyError(name string) error {
	c, ok := Lookup(name)
	if !ok {
		return fmt.Errorf("%s: not an admitted client", name)
	}
	return fmt.Errorf("%s not found on PATH and no managed archive release is admitted — %s", name, c.InstallHint)
}

// AssetMatch maps Go's goos/goarch onto a managed client's release asset.
// Only archives with a recorded digest match, so resolution is unambiguous.
func AssetMatch(name string) func(asset, goos, goarch string) bool {
	return func(asset, goos, goarch string) bool {
		c, ok := clients[name]
		if !ok || !c.Managed {
			return false
		}
		if _, ok := c.Digests[asset]; !ok {
			return false
		}
		a := strings.ToLower(asset)
		var arch, os []string
		switch name {
		case "stripe":
			os = map[string][]string{"darwin": {"mac-os"}, "linux": {"linux"}, "windows": {"windows"}}[goos]
			arch = map[string][]string{"amd64": {"x86_64"}, "arm64": {"arm64"}}[goarch]
		case "slack":
			os = map[string][]string{"darwin": {"macos"}, "linux": {"linux"}, "windows": {"windows"}}[goos]
			arch = map[string][]string{"amd64": {"amd64", "64-bit"}, "arm64": {"arm64"}}[goarch]
			if goos == "windows" {
				arch = []string{"64-bit"}
			}
		}
		if len(os) == 0 || len(arch) == 0 {
			return false
		}
		if goos == "windows" && goarch != "amd64" {
			return false
		}
		return strings.Contains(a, os[0]) && strings.Contains(a, "_"+arch[0])
	}
}

// ChildEnv returns env with each client's telemetry opt-out set. Only
// opt-outs documented by the vendor are set; Twilio and Slack have no
// verified switch (named gap in EVIDENCE.md).
func ChildEnv(name string, env []string) []string {
	var add []string
	switch name {
	case "sf":
		add = []string{"SF_DISABLE_TELEMETRY=true", "SF_AUTOUPDATE_DISABLE=true"}
	case "stripe":
		add = []string{"STRIPE_CLI_TELEMETRY_OPTOUT=1"}
	}
	return append(append([]string{}, env...), add...)
}

// Spec returns the binmgr GitHub spec for a managed client (stripe, slack).
// An empty version resolves the reviewed pin, never "latest".
func Spec(name, version string) (binmgrSpec, error) {
	c, ok := clients[name]
	if !ok || !c.Managed {
		return binmgrSpec{}, HostOnlyError(name)
	}
	if version == "" {
		version = c.Version
	}
	return binmgrSpec{Name: name, Repo: c.Repo, Version: version, Member: name, AssetMatch: AssetMatch(name)}, nil
}
