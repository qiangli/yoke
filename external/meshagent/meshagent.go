// Package meshagent resolves and execs the outpost mesh agent WITHOUT linking it,
// so bashy stays the standalone userland keystone. It is the shared plumbing
// behind the front-door verbs that drive the mesh — `bashy sphere` (tier 4) and
// `bashy tessaro`/`bashy login` (account) — each of which prints its own
// context-specific guidance when the agent is absent.
//
// The mesh/pairing data plane is owned by outpost (github.com/qiangli/outpost);
// this package only finds its binary and passes commands through, the same
// exec-never-link discipline as `bashy podman`/`kubectl`.
package meshagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// HostVersion supplies the embedding product's release tag. Set once at startup.
// An untagged development host falls back to latest; release tags retain their
// prerelease suffix so fetching a companion never switches release channels.
var HostVersion = func() string { return "" }

// Spec is the binmgr GitHub spec for the outpost member of a bashy release.
func Spec(version string) binmgr.GitHubSpec {
	version = strings.TrimSpace(version)
	if version == "" {
		version = strings.TrimSpace(os.Getenv("OUTPOST_VERSION"))
	}
	if version == "" {
		version = strings.TrimSpace(HostVersion())
	}
	if version == "" || version == "dev" || version == "(devel)" {
		version = "latest"
	}
	if version != "latest" && !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return binmgr.GitHubSpec{Name: "outpost", Repo: "qiangli/bashy", Version: version,
		AssetMatch: func(name, goos, goarch string) bool {
			suffix := "-" + goos + "-" + goarch
			if goos == "windows" {
				suffix += ".exe"
			}
			return strings.HasPrefix(name, "outpost-") && strings.HasSuffix(name, suffix)
		},
	}
}

// Ensure provisions the outpost mesh agent — download → sha256 → cache — and
// returns its path. This is what the apps
// console's Pair button runs on an unpaired host (sprint 220, story
// 72c86b58): the operator pastes an invite code and never opens a terminal.
func Ensure(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("outpost: resolve: %w", err)
	}
	return binmgr.Ensure(ctx, tool)
}

// ErrNotFound means the outpost mesh agent binary could not be located — the
// caller should print its own invite/guidance.
var ErrNotFound = errors.New("meshagent: outpost mesh agent not found")

// Resolve finds the outpost binary: executable sibling, $OUTPOST_BIN, $PATH, then the usual
// install spots. Returns ("", false) when none is usable.
func Resolve() (string, bool) {
	exe, _ := os.Executable()
	return resolve(exe)
}

// resolve accepts the host executable path so tests never need to execute themselves.
func resolve(exe string) (string, bool) {
	name := "outpost"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe != "" {
		// Follow the host link (e.g. Homebrew's bin link) to its versioned pair.
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		if p := filepath.Join(filepath.Dir(exe), name); isExec(p) {
			return p, true
		}
	}
	if p := strings.TrimSpace(os.Getenv("OUTPOST_BIN")); p != "" {
		return p, isExec(p)
	}
	if p, err := exec.LookPath("outpost"); err == nil && p != "" {
		return p, true
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, rel := range []string{filepath.Join("bin", name), filepath.Join(".local", "bin", name)} {
			if cand := filepath.Join(home, rel); isExec(cand) {
				return cand, true
			}
		}
	}
	// A copy Ensure provisioned earlier (the console's Pair button, or an
	// explicit provisioning) — bashy's own cache, last so an operator's
	// install on PATH keeps winning.
	if cached := binmgr.CachedBinary("outpost"); cached != "" && isExec(cached) {
		return cached, true
	}
	return "", false
}

// Installed reports whether the mesh agent is resolvable.
func Installed() bool {
	_, ok := Resolve()
	return ok
}

// Exec runs `outpost <args…>` with inherited stdio. Returns ErrNotFound when the
// agent is absent (so the caller can print its own guidance), or a clear error if
// $OUTPOST_BIN is set but not executable.
func Exec(ctx context.Context, args ...string) error {
	bin, ok := Resolve()
	if !ok {
		if p := strings.TrimSpace(os.Getenv("OUTPOST_BIN")); p != "" {
			return fmt.Errorf("meshagent: $OUTPOST_BIN=%q is not an executable", p)
		}
		return ErrNotFound
	}
	c := binmgr.Command(ctx, bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().IsRegular() && (runtime.GOOS == "windows" && strings.EqualFold(filepath.Ext(p), ".exe") || runtime.GOOS != "windows" && fi.Mode()&0o111 != 0)
}
