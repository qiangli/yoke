// Package rclone runs rclone (a pure-Go, MIT, sync/transfer multi-protocol tool)
// as a managed external binary (pkg/binmgr) — the transfer engine for the dhnt
// mesh directory-mirror (pkg/mirror) and a NAS-style file server. The rclone
// binary is downloaded → sha256-verified → cached by binmgr, never compiled in.
// rclone/rclone is MIT. See dhnt/docs/external-binary-builtins.md.
package rclone

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion pins the rclone release. "" / "latest" resolves the newest.
const DefaultVersion = "latest"

// Spec is the binmgr GitHub spec. rclone ships per-platform .zip archives whose
// binary is nested (rclone-<ver>-<plat>/rclone, matched by basename) and uses
// "osx" for macOS (custom AssetMatch); checksums come from its SHA256SUMS file.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{
		Name: "rclone", Repo: "rclone/rclone", Version: version,
		Member: "rclone", AssetMatch: assetMatch,
	}
}

// assetMatch maps Go's darwin onto rclone's "osx" token (arch stays Go-native:
// amd64/arm64) and matches the .zip for the current platform.
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	osTok := goos
	if goos == "darwin" {
		osTok = "osx"
	}
	return strings.Contains(n, osTok) && strings.Contains(n, goarch)
}

// Path resolves + caches the rclone binary, returning its path (used by
// pkg/mirror to drive `rclone sync`, and by Run).
func Path(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("rclone: resolve: %w", err)
	}
	p, err := binmgr.Ensure(ctx, tool)
	if err != nil {
		return "", fmt.Errorf("rclone: fetch: %w", err)
	}
	return p, nil
}

// Run execs the cached rclone with the given args, inheriting stdio — a
// transparent passthrough so `bashy rclone <any rclone command>` works (serve,
// sync, ls, …). Returns the child's exit code via the returned error.
func Run(ctx context.Context, args []string) error {
	bin, err := Path(ctx, "")
	if err != nil {
		return err
	}
	c := binmgr.Command(ctx, bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// NewRcloneCmd builds the `rclone` passthrough command (bashy front-door).
func NewRcloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "rclone [rclone args...]",
		Short:              "Run rclone (managed external binary) — sync/copy/serve over many protocols",
		Long:               "A transparent passthrough to a binmgr-managed rclone (MIT, pure Go). e.g. `bashy rclone serve webdav <dir>`, `bashy rclone sync <src> <dst>`. The mesh directory-mirror (bashy mirror) uses it as its transfer engine.",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), args)
		},
	}
	return cmd
}
