// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/external/notrack"
	"github.com/qiangli/yoke/pkg/binmgr"
	"mvdan.cc/sh/v3/pathconv"
)

// OpenClawNodeVersion is the Node.js provisioned to run the pinned OpenClaw
// CLI (pkg/binmgr openclaw@2026.10.2), whose engines floor is
// ">=24.16.0 <25 || >=26.1.0" (pkg/feed/doctor.go gates the same floor).
// The 24 LTS line is the stable choice over current 26. Verified through the
// existing tooling: Ensure fail-closes against nodejs.org's official
// SHASUMS256.txt for this version over HTTPS. Only DefaultVersion carries an
// embedded pin, so this version's trust anchor is that live official
// checksum, not a frozen digest — recorded honestly here.
const OpenClawNodeVersion = "24.21.0"

// npmProdMarker is the completion file written into an npm package dir after
// its production dependencies install cleanly. Its content names the exact
// tool@version and node version, so a bump of either reinstalls; its absence
// (or a node_modules wipe) reinstalls. A failed install never writes it, so a
// half-installed tree is retried, never trusted.
const npmProdMarker = ".bashy-npm-prod-installed"

// EnsureNpmTool provisions an integrity-verified npm-packaged CLI and its
// production dependency graph, and returns the tree entry script path.
//
// It reuses the existing tooling end to end: binmgr.Ensure extracts the
// pinned tarball (the committed pins.go sha256 stays authoritative over the
// asset digest), then the provisioned npm installs --omit=dev into the
// extracted package dir. Idempotent: a valid marker skips npm with no
// network I/O, so provisioned hosts run offline.
//
// nodeVersion selects the Node.js provisioned for both the install and the
// launch (empty means DefaultVersion); the caller must pass the CLI's
// engines floor (OpenClawNodeVersion for OpenClaw).
//
// The returned script is NOT directly executable cross-platform (Windows
// cannot exec .mjs): launch it with NpmToolCommand.
func EnsureNpmTool(ctx context.Context, tool binmgr.Tool, nodeVersion string) (string, error) {
	script, err := binmgr.Ensure(ctx, tool)
	if err != nil {
		return "", err
	}
	pkgDir, err := npmPackageDir(script)
	if err != nil {
		return "", err
	}
	_, binDir, err := Ensure(ctx, nodeVersion)
	if err != nil {
		return "", err
	}
	npm := filepath.Join(binDir, "npm")
	if runtime.GOOS == "windows" {
		npm += ".cmd"
	}
	if err := installNpmProd(ctx, npm, binDir, pkgDir, npmMarkerWant(tool, nodeVersionName(nodeVersion))); err != nil {
		return "", fmt.Errorf("node: npm install production deps for %s@%s: %w", tool.Name, tool.Version, err)
	}
	return script, nil
}

// installNpmProd installs the package dir's production graph with npm unless
// its completion marker already matches want (offline-safe skip). A failed
// install writes no marker, so a half-installed tree is retried, never
// trusted. npmBin points at the provisioned npm (tests use a stub).
func installNpmProd(ctx context.Context, npmBin, binDir, pkgDir, want string) error {
	if npmProdComplete(pkgDir, want) {
		return nil
	}
	// --legacy-peer-deps: the pinned release's own tree carries a
	// conflicting peer (2026.10.2: @mistralai/mistralai@2.7.0 wants
	// peerOptional @opentelemetry/exporter-trace-otlp-http ^0.220.0 while
	// the tree resolves 0.222.0 via dev @opentelemetry/sdk-node; strict npm
	// refuses with ERESOLVE). Upstream ships no overrides or lockfile, so a
	// strict install of this exact release is unresolvable by construction —
	// npm's own remediation. The conflict is a peerOptional telemetry
	// exporter, and the newer 0.222.0 stays; the smoke (CLI --version/--help)
	// proves the resolution runs. This affects version resolution only: the
	// tarball itself remains gated by the committed pin.
	argv := npmArgv(runtime.GOOS, pathconv.LogicalDrives(), npmBin, []string{
		"install", "--prefix", pkgDir,
		"--omit=dev", "--legacy-peer-deps",
		"--no-audit", "--no-fund", "--no-package-lock", "--loglevel=error",
	})
	// The extracted tree may carry bundled deps npm retires for replacement
	// but never refills (observed: npm 11 empties inBundle dirs and leaves
	// them empty). Stash them first so any outcome — success or failure —
	// ends with the pinned bytes back in place.
	stashed, bakRoot, err := stashBundledDeps(pkgDir)
	if err != nil {
		return err
	}
	runOut, runErr := runNpm(ctx, argv, npmEnv(binDir))
	if restoreErr := restoreEmptiedDeps(stashed); restoreErr != nil {
		return restoreErr
	}
	os.RemoveAll(bakRoot)
	if runErr != nil {
		return fmt.Errorf("%w: %s", runErr, tailLines(runOut, 5))
	}
	return os.WriteFile(filepath.Join(pkgDir, npmProdMarker), []byte(want), 0o644)
}

// stashedDep is one pre-existing top-level node_modules entry moved aside
// before npm install. On a fresh extract these are exactly the tarball's
// bundled deps; on a reinstall they are the previously provisioned tree.
type stashedDep struct {
	name    string // "undici" or "@openclaw/proxyline"
	liveDir string // node_modules path npm operates on
	bakDir  string // backup path holding the pinned bytes
	version string // version from the stashed package.json ("" if unreadable)
}

// stashBundledDeps moves every pre-existing top-level node_modules package
// aside into a hidden backup dir, returning the stash and its root. Dot
// entries (npm's own .bin/.package-lock.json bookkeeping) stay: npm owns
// those. An absent node_modules means nothing to protect.
func stashBundledDeps(pkgDir string) ([]stashedDep, string, error) {
	nm := filepath.Join(pkgDir, "node_modules")
	entries, err := os.ReadDir(nm)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	bakRoot, err := os.MkdirTemp(pkgDir, ".bashy-npm-stash-")
	if err != nil {
		return nil, "", err
	}
	var stashed []stashedDep
	stash := func(name, live string) error {
		dst := filepath.Join(bakRoot, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Rename(live, dst); err != nil {
			return err
		}
		stashed = append(stashed, stashedDep{name: name, liveDir: live, bakDir: dst, version: pkgVersion(dst)})
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !strings.HasPrefix(e.Name(), "@") {
			if err := stash(e.Name(), filepath.Join(nm, e.Name())); err != nil {
				return nil, "", err
			}
			continue
		}
		for _, sub := range readDirNames(filepath.Join(nm, e.Name())) {
			if strings.HasPrefix(sub, ".") {
				continue
			}
			if err := stash(filepath.Join(e.Name(), sub), filepath.Join(nm, e.Name(), sub)); err != nil {
				return nil, "", err
			}
		}
	}
	return stashed, bakRoot, nil
}

// restoreEmptiedDeps puts the pinned bytes back wherever npm left a missing
// or empty dir. A dir npm actually filled is left alone — but its version
// must still match the stash, otherwise npm silently swapped a bundled dep
// for different bytes and the tree is refused instead of shipped.
func restoreEmptiedDeps(stashed []stashedDep) error {
	for _, s := range stashed {
		if dirNonEmpty(s.liveDir) {
			if liveVer := pkgVersion(s.liveDir); liveVer != "" && s.version != "" && liveVer != s.version {
				return fmt.Errorf("node: npm replaced bundled %s %s with %s — refusing a tree that diverges from the pinned release (stash kept at %s)", s.name, s.version, liveVer, s.bakDir)
			}
			continue
		}
		os.RemoveAll(s.liveDir)
		if err := os.MkdirAll(filepath.Dir(s.liveDir), 0o755); err != nil {
			return err
		}
		if err := os.Rename(s.bakDir, s.liveDir); err != nil {
			return fmt.Errorf("node: restore bundled %s: %w (stash at %s)", s.name, err, s.bakDir)
		}
	}
	return nil
}

// pkgVersion reads the version from a package dir's package.json, or "".
func pkgVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Version)
}

func dirNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

func readDirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// NpmToolCommand returns `node <script> args...` for a provisioned npm-tree
// script — the cross-platform launcher (Windows cannot exec .mjs directly,
// and a bare exec would also miss the provisioned Node on PATH). The command
// carries the provisioned bin dir on PATH with tracking disabled, mirroring
// newExecCmd; the caller wires stdio. Ensure the tree first with
// EnsureNpmTool.
func NpmToolCommand(ctx context.Context, nodeVersion, script string, args ...string) (*exec.Cmd, error) {
	nodeBin, binDir, err := Ensure(ctx, nodeVersion)
	if err != nil {
		return nil, err
	}
	c := binmgr.Command(ctx, nodeBin, append([]string{script}, args...)...)
	c.Env = npmEnv(binDir)
	return c, nil
}

// nodeVersionName normalizes the requested version for the marker: empty
// means the default provision.
func nodeVersionName(v string) string {
	if strings.TrimSpace(v) == "" {
		return DefaultVersion
	}
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// npmMarkerWant is the exact marker content for this tool + node pair.
func npmMarkerWant(tool binmgr.Tool, nodeVersion string) string {
	return tool.Name + "@" + tool.Version + " node=" + nodeVersion + " prod"
}

// npmProdComplete reports whether pkgDir already holds a completed production
// install for want (marker matches and node_modules survived).
func npmProdComplete(pkgDir, want string) bool {
	data, err := os.ReadFile(filepath.Join(pkgDir, npmProdMarker))
	if err != nil || strings.TrimSpace(string(data)) != want {
		return false
	}
	st, err := os.Stat(filepath.Join(pkgDir, "node_modules"))
	return err == nil && st.IsDir()
}

// npmPackageDir finds the npm package dir (the one holding package.json)
// above the extracted entry script, bounded so a stray path fails closed
// instead of climbing to /.
func npmPackageDir(script string) (string, error) {
	dir := filepath.Dir(script)
	for i := 0; i < 4; i++ {
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("node: no package.json above %s (npm tree incomplete?)", script)
}

// npmEnv is the child env for npm and node-launched CLIs: provisioned bin
// dir first on PATH, tracking disabled.
func npmEnv(binDir string) []string {
	return notrack.Env(append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH")))
}

// runNpm executes the npm argv and returns combined output. A variable seam
// is unnecessary: tests point npm at a stub executable.
func runNpm(ctx context.Context, argv []string, env []string) (string, error) {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = env
	out, err := c.CombinedOutput()
	return string(out), err
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n {
		return strings.TrimSpace(s)
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
