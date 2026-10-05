// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package pwsh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/muslrt"
)

// Linux without glibc — the FROM-scratch bashy image, which has no libc at
// all — runs PowerShell on musl. Everything it needs beyond the PowerShell
// archive is pinned here per architecture and fetched on first use, the same
// way Python gets musl's loader:
//
//   - musl's loader at /lib (shared with Python, pkg/muslrt);
//   - libstdc++ and libgcc_s, which every native .NET part needs, and OpenSSL
//     (libssl/libcrypto), which .NET loads for crypto and TLS. They go to a
//     private directory in bashy's cache, never /lib, and only the PowerShell
//     child sees it through LD_LIBRARY_PATH. Each is skipped when musl's
//     default path already has it (an Alpine host).
//   - on arm64, which has no self-contained musl archive upstream: the
//     framework-dependent archive, the musl arm64 .NET runtime it runs on, and
//     Alpine's source-built musl libpsl-native placed next to pwsh.dll.
//
// ICU is not provisioned: .NET runs with invariant globalization there.
// Licences and the libstdc++/libgcc ruling: bashy/docs/fence-toolchain-licenses.md.
// `bashy self image --with pwsh` runs this same provisioning at image build.

// muslLibSet names the pinned library set; it is the cache key, so a pin
// change must change it.
const muslLibSet = "alpine3.24-1"

// dotnetRuntime is the .NET runtime each framework-dependent pin runs on: the
// version the self-contained archives of the same release carry. The sha256
// was computed from the download after it matched Microsoft's published
// sha512 (releases.json, 10.0.12).
var dotnetRuntime = map[string]struct{ version, url, sha256 string }{
	"7.6.6/linux/arm64/musl": {
		"10.0.12",
		"https://builds.dotnet.microsoft.com/dotnet/Runtime/10.0.12/dotnet-runtime-10.0.12-linux-musl-arm64.tar.gz",
		"8ff79d85ec4d4caa3b90bc6cfcce9bd28c3336cb245db3d6f9203c3fd77a2d4b",
	},
}

// muslLibs are the shared libraries the musl route provisions.
func muslLibs(goarch string) ([]muslrt.Lib, error) {
	pick := func(m map[string]muslrt.Package) (muslrt.Package, error) {
		p, ok := m[goarch]
		if !ok {
			return p, fmt.Errorf("pwsh: no musl runtime libraries pinned for %s", goarch)
		}
		return p, nil
	}
	var libs []muslrt.Lib
	for _, l := range []struct {
		soname, member string
		pins           map[string]muslrt.Package
	}{
		{"libstdc++.so.6", "usr/lib/libstdc++.so.6", muslrt.LibStdCxx},
		{"libgcc_s.so.1", "usr/lib/libgcc_s.so.1", muslrt.LibGcc},
		{"libcrypto.so.3", "usr/lib/libcrypto.so.3", muslrt.LibCrypto},
		{"libssl.so.3", "usr/lib/libssl.so.3", muslrt.LibSSL},
	} {
		p, err := pick(l.pins)
		if err != nil {
			return nil, err
		}
		libs = append(libs, muslrt.Lib{Soname: l.soname, Member: l.member, Pkg: p})
	}
	return libs, nil
}

// muslLibDir is the private library directory under bashy's cache (under
// /opt/bashy in the preloaded image, where BASHY_BIN_CACHE points).
func muslLibDir() (string, error) {
	root, err := binmgr.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "pwsh-musl-libs", muslLibSet, runtime.GOARCH), nil
}

func resolveMusl(ctx context.Context, version string, asset releaseAsset) (Launch, error) {
	if err := muslrt.EnsureLoader(ctx, "pwsh", "bashy self image --with pwsh"); err != nil {
		return Launch{}, err
	}
	libs, err := muslLibs(runtime.GOARCH)
	if err != nil {
		return Launch{}, err
	}
	dir, err := muslLibDir()
	if err != nil {
		return Launch{}, err
	}
	used, err := muslrt.EnsureLibs(ctx, dir, libs)
	if err != nil {
		return Launch{}, fmt.Errorf("pwsh: musl runtime libraries: %w", err)
	}
	var env []string
	if used {
		env = append(env, "LD_LIBRARY_PATH="+dir)
	}
	if os.Getenv("DOTNET_SYSTEM_GLOBALIZATION_INVARIANT") == "" {
		env = append(env, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	}
	if !asset.fxdependent {
		bin, err := ensureAsset(ctx, version, asset)
		if err != nil {
			return Launch{}, err
		}
		return Launch{Argv: []string{bin}, Env: env}, nil
	}

	key := platformKey(version, runtime.GOOS, runtime.GOARCH, true)
	rt, ok := dotnetRuntime[key]
	if !ok {
		return Launch{}, fmt.Errorf("pwsh: no .NET runtime pinned for %s", key)
	}
	dotnet, err := binmgr.Ensure(ctx, binmgr.Tool{
		Name: "dotnet-runtime-musl", Version: rt.version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: rt.url, SHA256: rt.sha256, Tree: true, Entrypoint: "dotnet"},
		},
	})
	if err != nil {
		return Launch{}, err
	}
	apphost, err := ensureAsset(ctx, version, asset)
	if err != nil {
		return Launch{}, err
	}
	app := filepath.Dir(apphost)
	// The archive's libpsl-native builds are glibc arm64 and musl x64. The
	// app directory is probed first, so Alpine's musl arm64 build goes there.
	psl, ok := muslrt.LibPslNative[runtime.GOARCH]
	if !ok {
		return Launch{}, fmt.Errorf("pwsh: no libpsl-native pinned for %s", runtime.GOARCH)
	}
	if err := ensureFile(ctx, filepath.Join(app, "libpsl-native.so"), psl, "usr/lib/libpsl-native.so"); err != nil {
		return Launch{}, fmt.Errorf("pwsh: libpsl-native: %w", err)
	}
	return Launch{Argv: []string{dotnet, filepath.Join(app, "pwsh.dll")}, Env: env}, nil
}

// ensureFile installs one member of a pinned package at dest unless present.
func ensureFile(ctx context.Context, dest string, pkg muslrt.Package, member string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	data, err := muslrt.Fetch(ctx, pkg)
	if err != nil {
		return err
	}
	body, mode, err := muslrt.File(data, member)
	if err != nil {
		return err
	}
	return muslrt.InstallFile(dest, body, mode|0o444)
}
