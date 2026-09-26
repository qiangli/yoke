// Package mise runs jdx/mise (the polyglot dev-tool / runtime version manager)
// as a managed external binary (pkg/binmgr): downloaded → verified → cached,
// never compiled in. `bashy mise …` is a transparent passthrough. mise is the
// power-user layer over bashy's native `go`/`node`/`python`/… provisioners:
// `.tool-versions` / `mise.toml`, many pinned versions, and the long tail of
// languages (ruby, deno, …). jdx/mise is MIT.
package mise

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion resolves the newest release; pin via $MISE_VERSION.
const DefaultVersion = "latest"

// Spec is the binmgr GitHub spec. jdx/mise ships per-platform archives
// (mise-v<ver>-<os>-<arch>.tar.gz for unix, .zip for windows) whose binary lives
// at mise/bin/mise inside the tree.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	entry := "mise/bin/mise"
	if platIsWindows() {
		entry = "mise/bin/mise.exe"
	}
	return binmgr.GitHubSpec{
		Name: "mise", Repo: "jdx/mise", Version: version,
		Tree: true, Entrypoint: entry, AssetMatch: assetMatch,
	}
}

func platIsWindows() bool { return strings.HasPrefix(binmgr.Platform(), "windows/") }

// assetMatch maps Go's goos/goarch onto mise's release tokens: os
// linux/macos/windows, arch x64 (amd64) / arm64, in a .tar.gz (unix) / .zip
// (windows) archive — excluding the raw-binary and checksum assets.
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	osTok := goos
	if goos == "darwin" {
		osTok = "macos"
	}
	archTok := goarch
	if goarch == "amd64" {
		archTok = "x64"
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return strings.Contains(n, osTok) &&
		strings.Contains(n, "-"+archTok+".") &&
		strings.HasSuffix(n, ext)
}

// Ensure fetches (if needed) the mise binary and returns its cached path.
func Ensure(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("mise: resolve: %w", err)
	}
	return binmgr.Ensure(ctx, tool)
}

// NewMiseCmd is the `bashy mise` front-door: a transparent passthrough.
func NewMiseCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "mise",
		Short:              "Polyglot runtime/version manager (jdx/mise, managed external binary)",
		Long:               "mise manages many language runtimes + tool versions from .tool-versions/mise.toml. Downloaded, verified, and cached by binmgr (not compiled in). $MISE_VERSION pins the release; all args pass through.",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := Ensure(cmd.Context(), os.Getenv("MISE_VERSION"))
			if err != nil {
				return err
			}
			c := binmgr.Command(cmd.Context(), bin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
}
