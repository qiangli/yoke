// Package govulncheck provisions and runs golang.org/x/vuln/cmd/govulncheck (Go's
// official vulnerability checker) built on demand at a pinned version with bashy's
// managed Go toolchain (pkg/binmgr / external/gotoolchain). Cached in the bashy tool
// cache so later runs do not rebuild. golang/vuln is BSD-3-Clause.
package govulncheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/gotoolchain"
	"github.com/qiangli/yoke/external/notrack"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion is the pinned govulncheck version.
const DefaultVersion = "v1.9.0"

// CacheDir returns the root directory for managed tools.
func CacheDir() (string, error) {
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" && strings.TrimSpace(os.Getenv("BASHY_BIN_CACHE")) == "" {
		return filepath.Join(h, "cache", "bin"), nil
	}
	return binmgr.CacheDir()
}

// CachedBinary returns the path to the cached govulncheck executable if present.
func CachedBinary(version string) string {
	if version == "" {
		version = DefaultVersion
	}
	root, err := CacheDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(root, "govulncheck", version, binmgr.BinaryName("govulncheck"))
	if binmgr.IsExecutable(p) {
		return p
	}
	return ""
}

func resolveGo(ctx context.Context) (goBin, goroot string, err error) {
	if bin, root, err := gotoolchain.Ensure(ctx, ""); err == nil {
		return bin, root, nil
	}
	if p, err := exec.LookPath("go"); err == nil {
		return p, runtime.GOROOT(), nil
	}
	return "", "", fmt.Errorf("managed Go toolchain unavailable")
}

// Ensure builds (if needed) the pinned govulncheck using bashy's managed Go and returns
// the cached executable path. Idempotent: once built, subsequent calls return the cached binary.
func Ensure(ctx context.Context, version string) (string, error) {
	if version == "" {
		version = DefaultVersion
	}
	if p := CachedBinary(version); p != "" {
		return p, nil
	}
	if binmgr.Offline() {
		return "", fmt.Errorf("binmgr: govulncheck %s is not cached; BASHY_OFFLINE=1 forbids building it", version)
	}

	cacheRoot, err := CacheDir()
	if err != nil {
		return "", fmt.Errorf("govulncheck: cache dir: %w", err)
	}
	targetDir := filepath.Join(cacheRoot, "govulncheck", version)
	binName := binmgr.BinaryName("govulncheck")
	binPath := filepath.Join(targetDir, binName)

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("govulncheck: mkdir: %w", err)
	}

	goBin, goroot, err := resolveGo(ctx)
	if err != nil {
		return "", fmt.Errorf("govulncheck: ensure managed go: %w", err)
	}

	cmd := binmgr.Command(ctx, goBin, "install", "golang.org/x/vuln/cmd/govulncheck@"+version)
	env := notrack.Env(os.Environ())
	env = append(env,
		"GOBIN="+targetDir,
		"GOWORK=off",
	)
	if goroot != "" {
		env = append(env, "GOROOT="+goroot)
	}
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("govulncheck: go install: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	if !binmgr.IsExecutable(binPath) {
		return "", fmt.Errorf("govulncheck: build succeeded but %s is not executable", binPath)
	}

	return binPath, nil
}

// ChildEnv returns env with tracking disabled.
func ChildEnv(env []string) []string { return notrack.Env(env) }

// NewGovulncheckCmd builds the `bashy govulncheck` front-door.
func NewGovulncheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "govulncheck",
		Short: "Run Go vulnerability checker (managed binary built with managed Go)",
		Long: `govulncheck reports known vulnerabilities that affect Go code or binary dependencies.
Built on demand with bashy's managed Go at a pinned version and cached by binmgr.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := Ensure(cmd.Context(), os.Getenv("GOVULNCHECK_VERSION"))
			if err != nil {
				return err
			}
			c := binmgr.Command(cmd.Context(), bin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			c.Env = notrack.Env(os.Environ())
			return c.Run()
		},
	}
}
