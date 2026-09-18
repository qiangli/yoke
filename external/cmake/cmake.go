// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package cmake provisions an official CMake on demand so `bashy cmake` works on
// a bare node with no system CMake: resolve the per-platform archive from
// Kitware's GitHub release, verify it against the published SHA-256 file, then
// hand off to binmgr's tree-mode Ensure (download → verify → extract → cache →
// exec). No embedding. CMake is BSD-3-Clause, and as a downloaded build tool it
// sits outside the compiled-in permissive rule anyway — it is a separate binary
// on its own license. This is half of the self-contained cross-platform build
// userland (the other half is external/clang).
package cmake

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion is used when no version is requested. Bump in step with a
// known-good Kitware release.
const DefaultVersion = "4.3.3"

const releaseBase = "https://github.com/Kitware/CMake/releases/download"

// asset returns the release archive filename and the `cmake` entrypoint path
// within the extracted tree, for the given "goos/goarch" platform. Kitware ships
// universal macOS archives, x86_64/aarch64 Linux, and x86_64/arm64 Windows zips.
func asset(plat, version string) (archive, entrypoint string, err error) {
	switch plat {
	case "darwin/amd64", "darwin/arm64":
		d := "cmake-" + version + "-macos-universal"
		return d + ".tar.gz", d + "/CMake.app/Contents/bin/cmake", nil
	case "linux/amd64":
		d := "cmake-" + version + "-linux-x86_64"
		return d + ".tar.gz", d + "/bin/cmake", nil
	case "linux/arm64":
		d := "cmake-" + version + "-linux-aarch64"
		return d + ".tar.gz", d + "/bin/cmake", nil
	case "windows/amd64":
		d := "cmake-" + version + "-windows-x86_64"
		return d + ".zip", d + "/bin/cmake.exe", nil
	case "windows/arm64":
		d := "cmake-" + version + "-windows-arm64"
		return d + ".zip", d + "/bin/cmake.exe", nil
	}
	return "", "", fmt.Errorf("cmake: no release asset for %s", plat)
}

// Ensure makes the requested CMake available and returns the path to its `cmake`
// executable. Idempotent: a cache hit does no network I/O. version is a bare
// number like "4.3.3" (a leading "v" is tolerated); empty means DefaultVersion.
func Ensure(ctx context.Context, version string) (string, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		version = DefaultVersion
	}
	archive, entry, err := asset(binmgr.Platform(), version)
	if err != nil {
		return "", err
	}
	// Fast path: a cached tree short-circuits before any network round-trip
	// (binmgr returns the entrypoint on a cache hit before downloading).
	probe := binmgr.Tool{
		Name: "cmake", Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: "cached", Tree: true, Entrypoint: entry},
		},
	}
	if p, perr := binmgr.Ensure(ctx, probe); perr == nil {
		return p, nil
	}
	sha, err := resolveSHA(ctx, version, archive)
	if err != nil {
		return "", err
	}
	tool := binmgr.Tool{
		Name: "cmake", Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {
				URL:        releaseBase + "/v" + version + "/" + archive,
				SHA256:     sha,
				Tree:       true,
				Entrypoint: entry,
			},
		},
	}
	return binmgr.Ensure(ctx, tool)
}

// resolveSHA fetches Kitware's `cmake-<ver>-SHA-256.txt` and returns the digest
// for the given archive filename. The file is "<sha256>  <filename>" lines — the
// verify is the trust boundary, so a missing entry is a hard error.
func resolveSHA(ctx context.Context, version, archive string) (string, error) {
	url := releaseBase + "/v" + version + "/cmake-" + version + "-SHA-256.txt"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cmake: fetch checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cmake: checksums HTTP %d for %s", resp.StatusCode, url)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == archive {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("cmake: %s not listed in %s", archive, url)
}

// NewCmakeCmd is the `bashy cmake` front-door: ensure the toolchain (version from
// $BASHY_CMAKE_VERSION or the default), then exec it with the user's args. Flags
// are passed through untouched to the real cmake.
func NewCmakeCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "cmake",
		Short:              "Run CMake, auto-provisioned (download + cache, no system CMake needed)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := Ensure(cmd.Context(), os.Getenv("BASHY_CMAKE_VERSION"))
			if err != nil {
				return err
			}
			c := exec.CommandContext(cmd.Context(), bin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
}
