// Package osvscanner runs google/osv-scanner (vulnerability scanner for dependencies
// and licenses against the OSV database) as a managed external binary (pkg/binmgr):
// downloaded → sha256-verified → cached, never compiled in.
// osv-scanner is Apache-2.0.
package osvscanner

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/notrack"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion is the pinned release version for osv-scanner.
const DefaultVersion = "v2.7.0"

// Spec is the binmgr GitHub spec for osv-scanner.
// google/osv-scanner publishes raw executables per platform:
// osv-scanner_darwin_amd64, osv-scanner_darwin_arm64, osv-scanner_linux_amd64,
// osv-scanner_linux_arm64, osv-scanner_windows_amd64.exe.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{
		Name:       "osv-scanner",
		Repo:       "google/osv-scanner",
		Version:    version,
		Member:     "",
		AssetMatch: assetMatch,
	}
}

// assetMatch maps Go's goos/goarch to osv-scanner's release asset naming.
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	if !strings.HasPrefix(n, "osv-scanner_") {
		return false
	}
	if strings.HasSuffix(n, ".jsonl") || strings.Contains(n, "sha256") || strings.Contains(n, "sums") {
		return false
	}
	if !strings.Contains(n, "_"+goos+"_") {
		return false
	}
	if goos == "windows" {
		return strings.HasSuffix(n, "_"+goarch+".exe")
	}
	return strings.HasSuffix(n, "_"+goarch)
}

// Ensure fetches (if needed) the osv-scanner binary and returns its cached path.
func Ensure(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("osv-scanner: resolve: %w", err)
	}
	return binmgr.Ensure(ctx, tool)
}

// ChildEnv returns env with tracking disabled.
func ChildEnv(env []string) []string { return notrack.Env(env) }

// NewOsvScannerCmd builds the `bashy osv-scanner` front-door.
func NewOsvScannerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "osv-scanner",
		Short: "Run google/osv-scanner (managed external binary)",
		Long: `osv-scanner finds vulnerabilities in dependencies and licenses against OSV — downloaded,
sha256-verified, and cached by binmgr (not compiled in). $OSV_SCANNER_VERSION pins the release;
all args pass through to osv-scanner.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := Ensure(cmd.Context(), os.Getenv("OSV_SCANNER_VERSION"))
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
