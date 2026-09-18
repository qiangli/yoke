// Package curlbin provides `bashy curl`. curl is near-universal — built into
// Windows 10+ (System32\curl.exe) and present on every unix — so `bashy curl`
// first uses the platform curl. On a bare Windows node with none, it provisions
// the official curl.se/windows build (pinned + sha256-verified). The upstream
// curl/curl repo ships source only; the canonical Windows BINARIES are the
// curl.se/windows builds. curl is MIT-style; downloaded + run, never linked.
package curlbin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// Pinned curl.se/windows build (win64), used only as the bare-node fallback.
const (
	curlWinVer   = "8.21.0_2"
	curlWinURL   = "https://curl.se/windows/dl-8.21.0_2/curl-8.21.0_2-win64-mingw.zip"
	curlWinSHA   = "2a3e951f522be7d9a3a964be57e74ee15d19dd970bcc7b7843445089901a3ed4"
	curlWinEntry = "curl-8.21.0_2-win64-mingw/bin/curl.exe"
)

// Ensure returns a path to curl: the platform curl if present (the common case),
// else the pinned checksum-verified curl.se/windows build on a bare Windows node.
func Ensure(ctx context.Context) (string, error) {
	if p, err := exec.LookPath("curl"); err == nil {
		return p, nil
	}
	if runtime.GOOS != "windows" {
		return "", fmt.Errorf("curl: not found on PATH (install it, or use `bashy fetch`)")
	}
	if runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("curl: the pinned Windows build is amd64-only (got %s)", runtime.GOARCH)
	}
	tool := binmgr.Tool{
		Name: "curl", Version: curlWinVer,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: curlWinURL, SHA256: curlWinSHA, Tree: true, Entrypoint: curlWinEntry},
		},
	}
	return binmgr.Ensure(ctx, tool)
}

// NewCurlCmd is the `bashy curl` front-door: resolve curl, then exec it.
func NewCurlCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "curl",
		Short:              "curl — the platform curl, or a pinned+verified curl.se/windows build on a bare Windows node",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := Ensure(cmd.Context())
			if err != nil {
				return err
			}
			p := exec.CommandContext(cmd.Context(), c, args...)
			p.Stdin, p.Stdout, p.Stderr = os.Stdin, os.Stdout, os.Stderr
			return p.Run()
		},
	}
}
