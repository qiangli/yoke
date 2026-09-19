// Package zigcc provisions a self-contained C toolchain for building the
// external POSIX providers (see tools/posix-providers/).
//
// Why Zig and not LLVM: the POSIX providers bashy does not implement in Go are
// all copyleft, so bashy may BUILD them locally but must never distribute their
// binaries (docs/posix-provider-distribution-policy.md). Building locally needs
// a C compiler on every platform, and the obvious candidate is too big — the
// official clang+llvm releases are 1.0-1.6 GB per platform, to produce a make(1)
// of a few hundred kilobytes. Zig ships the same clang frontend with bundled
// libc headers and a linker in 48-54 MB, roughly 27x smaller, and it is MIT
// licensed so bashy may fetch and cache it freely.
//
// This deliberately does NOT touch external/clang. That provider answers a
// different question ("give me clang": llvm-mingw on Windows, system clang
// elsewhere) and other callers depend on it. zigcc answers "give me a C
// compiler that is present on any platform without the host providing one".
package zigcc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// zigVersion is the pinned Zig release. Bump the version and every digest
// together; ziglang.org publishes a machine-readable index with per-asset
// shasums, so a bump is mechanical and verifiable.
const zigVersion = "0.16.0"

type zigRelease struct {
	URL    string
	SHA256 string
	Entry  string
}

// zigAsset pins one self-contained toolchain per supported platform. A platform
// absent from this map is REFUSED rather than silently falling back to a host
// compiler: an unrecorded toolchain would make a locally built provider
// unattributable.
var zigAsset = map[string]zigRelease{
	"linux/amd64": {
		URL:    "https://ziglang.org/download/0.16.0/zig-x86_64-linux-0.16.0.tar.xz",
		SHA256: "70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00",
		Entry:  "zig-x86_64-linux-0.16.0/zig",
	},
	"linux/arm64": {
		URL:    "https://ziglang.org/download/0.16.0/zig-aarch64-linux-0.16.0.tar.xz",
		SHA256: "ea4b09bfb22ec6f6c6ceac57ab63efb6b46e17ab08d21f69f3a48b38e1534f17",
		Entry:  "zig-aarch64-linux-0.16.0/zig",
	},
	"darwin/arm64": {
		URL:    "https://ziglang.org/download/0.16.0/zig-aarch64-macos-0.16.0.tar.xz",
		SHA256: "b23d70deaa879b5c2d486ed3316f7eaa53e84acf6fc9cc747de152450d401489",
		Entry:  "zig-aarch64-macos-0.16.0/zig",
	},
	"darwin/amd64": {
		URL:    "https://ziglang.org/download/0.16.0/zig-x86_64-macos-0.16.0.tar.xz",
		SHA256: "0387557ed1877bc6a2e1802c8391953baddba76081876301c522f52977b52ba7",
		Entry:  "zig-x86_64-macos-0.16.0/zig",
	},
	"windows/amd64": {
		URL:    "https://ziglang.org/download/0.16.0/zig-x86_64-windows-0.16.0.zip",
		SHA256: "68659eb5f1e4eb1437a722f1dd889c5a322c9954607f5edcf337bc3684a75a7e",
		Entry:  "zig-x86_64-windows-0.16.0/zig.exe",
	},
}

// Supported reports whether a pinned toolchain exists for a platform. Callers
// check this before a network attempt so an unsupported host fails fast and
// explains itself, rather than failing deep inside a download.
func Supported(platform string) bool {
	_, ok := zigAsset[platform]
	return ok
}

// Ensure downloads, verifies and caches the pinned Zig toolchain for this
// platform and returns the path to the `zig` executable. It is idempotent: a
// cached toolchain is returned without touching the network.
func Ensure(ctx context.Context) (string, error) {
	plat := binmgr.Platform()
	a, ok := zigAsset[plat]
	if !ok {
		return "", fmt.Errorf("zigcc: no pinned Zig toolchain for %s; add one to zigAsset before building POSIX providers there", plat)
	}
	tool := binmgr.Tool{
		Name:    "zig",
		Version: zigVersion,
		Assets: map[string]binmgr.Asset{
			plat: {URL: a.URL, SHA256: a.SHA256, Tree: true, Entrypoint: a.Entry},
		},
	}
	return binmgr.Ensure(ctx, tool)
}

// CC returns the argv prefix that behaves as a C compiler, for use as $CC in a
// provider build. Zig exposes its C frontend as a subcommand, so the compiler
// is two words rather than one; callers must not assume argv[0] alone.
func CC(ctx context.Context) ([]string, error) {
	bin, err := Ensure(ctx)
	if err != nil {
		return nil, err
	}
	return []string{bin, "cc"}, nil
}

// CXX is CC's C++ counterpart: zig's clang++ driver, same bundled libc++.
func CXX(ctx context.Context) ([]string, error) {
	bin, err := Ensure(ctx)
	if err != nil {
		return nil, err
	}
	return []string{bin, "c++"}, nil
}

// Linker returns a single program that behaves as `cc` — a one-line wrapper
// over `zig cc` written next to the cached zig — for callers that can name a
// linker but not an argv (rustc -C linker=). Idempotent.
func Linker(ctx context.Context) (string, error) {
	bin, err := Ensure(ctx)
	if err != nil {
		return "", err
	}
	wrapper := filepath.Join(filepath.Dir(bin), "zig-cc")
	body := "#!/bin/sh\nexec \"" + bin + "\" cc \"$@\"\n"
	if runtime.GOOS == "windows" {
		wrapper += ".cmd"
		body = "@\"" + bin + "\" cc %*\r\n"
	}
	if data, err := os.ReadFile(wrapper); err == nil && string(data) == body {
		return wrapper, nil
	}
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		return "", fmt.Errorf("zigcc: write linker wrapper: %w", err)
	}
	return wrapper, nil
}

// SystemFallback reports a host C compiler when one exists. It is only for
// callers that explicitly accept an unpinned toolchain — a certification build
// must not use it, because the compiler would then vary per host and the
// resulting provider could not be attributed to a recorded input.
func SystemFallback() (string, bool) {
	for _, c := range []string{"cc", "clang", "gcc"} {
		if p, err := exec.LookPath(c); err == nil {
			return p, true
		}
	}
	return "", false
}
