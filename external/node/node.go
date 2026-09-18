// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package node provisions an official Node.js runtime on demand so `bashy node`
// (and `npm`/`npx` via the same tree) work on a bare node with no system Node:
// resolve the platform archive + its sha256 from nodejs.org's per-release
// SHASUMS256.txt, then hand off to binmgr's tree-mode Ensure (download → verify
// → extract → cache → exec). No embedding — the self-sufficient worker story,
// same shape as external/gotoolchain.
package node

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// embeddedSums pins the OFFICIAL sha256 digests of the DefaultVersion archives,
// downloaded from nodejs.org in dev and checked in. This is the supply-chain
// trust anchor: the pinned digest ships INSIDE bashy (not fetched from the same
// origin as the binary at install time), so a compromised nodejs.org release
// can't hand us a matching-but-tampered checksum. `TestEmbeddedSumsMatchUpstream`
// re-downloads the official SHASUMS256 and fails if these drift — so a version
// bump is a deliberate, reviewed change. Non-default versions fall back to a
// live SHASUMS256 fetch (still fail-closed in binmgr).
//
//go:embed node-22.23.2.sums
var embeddedSums string

// pinnedSHA returns the embedded (pinned, offline) sha256 for filename, or "".
func pinnedSHA(filename string) string {
	sc := bufio.NewScanner(strings.NewReader(embeddedSums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == filename {
			return f[0]
		}
	}
	return ""
}

// DefaultVersion is the Node.js provisioned when none is requested. Active LTS.
//
// THIS PIN IS COUPLED TO pkg/meet/web/package.json's `packageManager` field, and
// the coupling is easy to miss because the two live in different trees. corepack
// honours that field, so `bashy pnpm` runs the pnpm the SPA asks for — and a pnpm
// release can raise its own Node floor. When 22.11.0 was pinned here, pnpm@11.17.0
// began requiring Node >= 22.13, so `bashy pnpm` failed on every clean provision
// with "This version of pnpm requires at least Node.js v22.13" and the meet SPA
// could not be built on a machine with no system Node — which is exactly the
// machine this provisioner exists for.
//
// So: when bumping the SPA's packageManager, check its Node floor against this
// constant. TestPnpmFloorSatisfiedByDefaultNode pins the relationship.
const DefaultVersion = "22.23.2"

// nodePlatform maps Go's GOOS/GOARCH to Node's dist naming (os, arch, ext).
func nodePlatform() (nos, narch, ext string, err error) {
	switch runtime.GOOS {
	case "linux":
		nos = "linux"
	case "darwin":
		nos = "darwin"
	case "windows":
		nos = "win"
	default:
		return "", "", "", fmt.Errorf("node: unsupported OS %q", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64":
		narch = "x64"
	case "arm64":
		narch = "arm64"
	default:
		return "", "", "", fmt.Errorf("node: unsupported arch %q", runtime.GOARCH)
	}
	ext = ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return nos, narch, ext, nil
}

// entrypoint is the node executable path inside the extracted tree.
func entrypoint(base string) string {
	if runtime.GOOS == "windows" {
		return base + "/node.exe"
	}
	return base + "/bin/node"
}

// Ensure makes the requested Node.js available and returns the path to its
// `node` executable plus its bin dir (so npm/npx are reachable). Idempotent: a
// cache hit does no network I/O. version is a bare number ("22.11.0"; a leading
// "v" is tolerated); empty means DefaultVersion.
func Ensure(ctx context.Context, version string) (nodeBin, binDir string, err error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		version = DefaultVersion
	}
	nos, narch, ext, err := nodePlatform()
	if err != nil {
		return "", "", err
	}
	base := fmt.Sprintf("node-v%s-%s-%s", version, nos, narch)
	entry := entrypoint(base)

	// Fast path: cached tree short-circuits before any network I/O.
	probe := binmgr.Tool{
		Name: "node", Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: "cached", Tree: true, Entrypoint: entry},
		},
	}
	if p, perr := binmgr.Ensure(ctx, probe); perr == nil {
		return p, filepath.Dir(p), nil
	}

	filename := base + ext
	url := fmt.Sprintf("https://nodejs.org/dist/v%s/%s", version, filename)
	sha, err := resolveSHA(ctx, version, filename)
	if err != nil {
		return "", "", err
	}
	tool := binmgr.Tool{
		Name: "node", Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: url, SHA256: sha, Tree: true, Entrypoint: entry},
		},
	}
	nodeBin, err = binmgr.Ensure(ctx, tool)
	if err != nil {
		return "", "", err
	}
	return nodeBin, filepath.Dir(nodeBin), nil
}

// resolveSHA fetches the release's SHASUMS256.txt and returns the sha256 of the
// named archive (each line is "<sha>  <filename>").
func resolveSHA(ctx context.Context, version, filename string) (string, error) {
	// Prefer the embedded pinned digest (offline, tamper-anchored). Only reach
	// nodejs.org for versions we didn't ship a pin for.
	if sha := pinnedSHA(filename); sha != "" {
		return sha, nil
	}
	fmt.Fprintf(os.Stderr, "note: node %s is not a pinned default (%s) — verifying against the official published checksum\n", version, DefaultVersion)
	url := fmt.Sprintf("https://nodejs.org/dist/v%s/SHASUMS256.txt", version)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("node: fetch SHASUMS256: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("node: SHASUMS256 for v%s HTTP %d (version not found?)", version, resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == filename {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("node: read SHASUMS256: %w", err)
	}
	return "", fmt.Errorf("node: %s not listed in SHASUMS256 for v%s (unsupported platform?)", filename, version)
}

// NewNodeCmd is the `bashy node` front-door: ensure the runtime (version from
// $BASHY_NODE_VERSION or the default), then exec it with the user's args. The
// node bin dir is prepended to PATH so a script that shells out to npm/npx finds
// the matching tools. Flags pass through untouched.
func NewNodeCmd() *cobra.Command {
	return newExecCmd("node", "Node.js runtime", "node")
}

// NewNpmCmd / NewNpxCmd are siblings that exec npm/npx from the same tree.
func NewNpmCmd() *cobra.Command { return newExecCmd("npm", "Node.js package manager", "npm") }
func NewNpxCmd() *cobra.Command { return newExecCmd("npx", "Node.js package runner", "npx") }

// NewPnpmCmd / NewYarnCmd route through corepack (shipped with Node ≥16.9), so
// pnpm/yarn are provisioned from the same Node tree with no extra download.
func NewPnpmCmd() *cobra.Command { return newCorepackCmd("pnpm") }
func NewYarnCmd() *cobra.Command { return newCorepackCmd("yarn") }

// newCorepackCmd builds a passthrough that provisions Node then runs a
// corepack-managed package manager (pnpm/yarn) via `corepack <pm> …`.
func newCorepackCmd(pm string) *cobra.Command {
	return &cobra.Command{
		Use:                pm,
		Short:              "Run " + pm + " via corepack, auto-provisioned (no system Node needed)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, binDir, err := Ensure(cmd.Context(), os.Getenv("BASHY_NODE_VERSION"))
			if err != nil {
				return err
			}
			corepack := filepath.Join(binDir, "corepack")
			if runtime.GOOS == "windows" {
				corepack += ".cmd"
			}
			c := exec.CommandContext(cmd.Context(), corepack, append([]string{pm}, args...)...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			c.Env = append(os.Environ(),
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"COREPACK_ENABLE_DOWNLOAD_PROMPT=0",
			)
			return c.Run()
		},
	}
}

// newExecCmd builds a passthrough cobra command that provisions Node then execs
// `tool` (node/npm/npx) from the provisioned tree.
func newExecCmd(use, short, tool string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              "Run " + short + ", auto-provisioned (download + cache, no system Node needed)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeBin, binDir, err := Ensure(cmd.Context(), os.Getenv("BASHY_NODE_VERSION"))
			if err != nil {
				return err
			}
			target := nodeBin
			if tool != "node" {
				// npm/npx live next to node; on Windows they are .cmd shims.
				name := tool
				if runtime.GOOS == "windows" {
					name = tool + ".cmd"
				}
				target = filepath.Join(binDir, name)
			}
			c := exec.CommandContext(cmd.Context(), target, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			c.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			return c.Run()
		},
	}
}
