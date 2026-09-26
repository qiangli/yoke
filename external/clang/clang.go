// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package clang provisions a standalone C/C++ toolchain on demand so `bashy
// clang` works on a bare node. On Windows — the bare-machine case that motivated
// this — it fetches the self-contained llvm-mingw toolchain (clang + lld +
// mingw-w64 headers/libs: native Windows binaries, no MSVC or Windows SDK
// needed) via binmgr (download → sha256-verify → extract → cache → exec). On
// macOS/Linux, where a system clang/gcc universally ships, it execs the PATH
// clang. LLVM is Apache-2.0 and the mingw-w64 runtime is permissive; as a
// downloaded build tool it sits outside the compiled-in rule regardless. This is
// the compiler half of the self-contained cross-platform build userland
// (external/cmake is the other half).
package clang

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// llvm-mingw release pinned for the Windows toolchain. mstorsjo/llvm-mingw
// publishes no checksum file, so the per-asset SHA-256 is pinned here (computed
// from the published .zip). Bump version + sha together.
const llvmMingwVersion = "20260616"

const llvmMingwBase = "https://github.com/mstorsjo/llvm-mingw/releases/download"

// winAsset maps a Windows platform to (mingw arch token, pinned sha256) for the
// llvm-mingw `ucrt` (modern Windows runtime) archives.
var winAsset = map[string]struct{ arch, sha256 string }{
	"windows/amd64": {"x86_64", "b9b68a4d276e16fa25802aaba458e4638f64b3884c290aaccdc2d87083b6ca35"},
	// windows/arm64: pin llvm-mingw-<ver>-ucrt-aarch64.zip when an arm64 host appears.
}

// Ensure returns the path to a `clang` executable for the current platform. On
// Windows it provisions the standalone llvm-mingw toolchain (download → verify →
// cache, idempotent). On macOS/Linux it resolves the system clang from PATH (one
// universally ships with Xcode CLT / the distro).
func Ensure(ctx context.Context) (string, error) {
	plat := binmgr.Platform()
	if a, ok := winAsset[plat]; ok {
		dir := "llvm-mingw-" + llvmMingwVersion + "-ucrt-" + a.arch
		tool := binmgr.Tool{
			Name: "clang", Version: llvmMingwVersion,
			Assets: map[string]binmgr.Asset{
				plat: {
					URL:        llvmMingwBase + "/" + llvmMingwVersion + "/" + dir + ".zip",
					SHA256:     a.sha256,
					Tree:       true,
					Entrypoint: dir + "/bin/clang.exe",
				},
			},
		}
		return binmgr.Ensure(ctx, tool)
	}
	// macOS/Linux: a system clang is universally present.
	if p, err := exec.LookPath("clang"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("clang: no system clang on %s — install Xcode Command Line Tools (macOS) or your distro's clang/llvm; the fetched llvm-mingw toolchain is provided only for Windows targets", plat)
}

// NewClangCmd is the `bashy clang` front-door: ensure the toolchain, then exec it
// with the user's args. Flags pass through untouched to the real clang.
func NewClangCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "clang",
		Short:              "Run Clang, auto-provisioned (download + cache on Windows; system clang on macOS/Linux)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := Ensure(cmd.Context())
			if err != nil {
				return err
			}
			c := binmgr.Command(cmd.Context(), bin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
}
