// Package gh runs the GitHub CLI (cli/cli) as a managed external binary
// (pkg/binmgr): downloaded → sha256-verified → cached, never compiled in.
// `bashy gh …` is a transparent passthrough — it ensures the gh binary, then
// execs it with your args. Together with `bashy act` (run workflows locally),
// `bashy go` (build/test), and the built-in git, gh closes the CI/CD loop
// entirely inside bashy: clone → build/test → act (local) → gh (PRs, trigger +
// watch the real github runs, `gh api`) → push/merge. cli/cli is MIT.
//
// Part of the "every good Go tool becomes a bashy ext" direction — see
// dhnt/docs/external-binary-builtins.md + local-p2p-cicd.md.
package gh

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion resolves the newest release; pin via $GH_VERSION / --version.
const DefaultVersion = "latest"

// Spec is the binmgr GitHub spec. cli/cli ships per-platform archives
// (gh_<ver>_linux_amd64.tar.gz, gh_<ver>_macOS_arm64.zip, gh_<ver>_windows_amd64.zip)
// with the binary nested at `gh_<ver>_<os>_<arch>/bin/gh` (matched by basename;
// the Windows `.exe` is handled by binmgr). The darwin→macOS token is mapped in
// AssetMatch.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{
		Name: "gh", Repo: "cli/cli", Version: version,
		Member: "gh", AssetMatch: assetMatch,
	}
}

// assetMatch maps Go's goos onto gh's release tokens (darwin→macos; arch tokens
// amd64/arm64 are Go-standard).
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	osTok := goos
	if goos == "darwin" {
		osTok = "macos"
	}
	if !strings.Contains(n, osTok) {
		return false
	}
	return strings.Contains(n, goarch)
}

// Ensure fetches (if needed) the gh binary and returns its cached path.
func Ensure(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("gh: resolve: %w", err)
	}
	return binmgr.Ensure(ctx, tool)
}

// NewGhCmd is the `bashy gh` front-door: a transparent passthrough to the managed
// GitHub CLI. $GH_VERSION pins the release; all args pass through to gh.
func NewGhCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gh",
		Short: "Run the GitHub CLI (cli/cli) as a managed external binary",
		Long: `gh runs the GitHub CLI — downloaded, sha256-verified, and cached by binmgr
(not compiled in). All args pass through to gh ($GH_VERSION pins the release).
With bashy act + go + git it closes the CI/CD loop without any system tooling.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := Ensure(cmd.Context(), os.Getenv("GH_VERSION"))
			if err != nil {
				return err
			}
			c := binmgr.Command(cmd.Context(), bin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
}
