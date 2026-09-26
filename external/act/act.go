// Package act runs nektos/act (run GitHub Actions locally) as a managed external
// binary (pkg/binmgr): downloaded → sha256-verified → cached, never compiled in.
// `bashy act …` is a transparent passthrough — it ensures the act binary, then
// execs it with your args. act runs `.github/workflows/*.yml` in containers, so
// it is a unix-HOST capability (needs a container engine — `bashy podman`); it's
// how you test CI locally / on a mesh node before pushing. nektos/act is MIT.
//
// First instance of the broader "every good Go tool becomes a bashy ext" idea —
// see dhnt/docs/external-binary-builtins.md + local-p2p-cicd.md.
package act

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion resolves the newest release; pin via $ACT_VERSION / --version
// for reproducibility.
const DefaultVersion = "latest"

// Spec is the binmgr GitHub spec. nektos/act ships per-platform archives
// (act_Linux_x86_64.tar.gz, act_Darwin_arm64.tar.gz, …) with the `act` binary at
// the archive root; the non-Go arch token (x86_64) is handled by AssetMatch.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{
		Name: "act", Repo: "nektos/act", Version: version,
		Member: "act", AssetMatch: assetMatch,
	}
}

// assetMatch maps Go's goos/goarch onto nektos/act's release tokens (amd64→
// x86_64; the OS token matches case-insensitively: Linux/Darwin/Windows).
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	if !strings.Contains(n, goos) {
		return false
	}
	archTok := goarch
	if goarch == "amd64" {
		archTok = "x86_64"
	}
	return strings.Contains(n, archTok)
}

// Ensure fetches (if needed) the act binary and returns its cached path.
func Ensure(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("act: resolve: %w", err)
	}
	return binmgr.Ensure(ctx, tool)
}

// NewActCmd is the `bashy act` front-door: a transparent passthrough to the
// managed act binary. All args pass through to act unchanged.
func NewActCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "act",
		Short: "Run GitHub Actions locally via nektos/act (managed external binary)",
		Long: `act runs your .github/workflows/*.yml locally in containers — downloaded,
sha256-verified, and cached by binmgr (not compiled in). Needs a container engine
(bashy podman on a unix host). $ACT_VERSION pins the release; all args pass
through to act.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := Ensure(cmd.Context(), os.Getenv("ACT_VERSION"))
			if err != nil {
				return err
			}
			c := binmgr.Command(cmd.Context(), bin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
}
