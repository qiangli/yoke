package python

import (
	"context"

	"github.com/qiangli/yoke/pkg/muslrt"
)

// On a Linux host without glibc — the FROM-scratch bashy image — the only
// CPython builds that can run are python-build-standalone's musl builds, and
// those are dynamically linked against musl: their ELF interpreter is
// /lib/ld-musl-<arch>.so.1. The host has no libc at all, so bashy provisions
// musl's loader (MIT) there from Alpine's pinned musl package, the same way it
// provisions every other toolchain: download, sha256-verify, install. When the
// root filesystem does not allow it (read-only, not root), the error names the
// way out: the preloaded python image variant. The loader and its pin are
// shared with the PowerShell runtime (pkg/muslrt).

// MuslUvVersion is the uv release used on hosts without glibc: UV_LIBC (the
// libc override that makes uv work where it cannot detect one) arrived in uv
// 0.7.22. glibc and macOS hosts keep DefaultVersion.
const MuslUvVersion = "0.12.19"

// needsMusl reports whether this host needs the musl path: Linux without glibc.
func needsMusl() bool { return muslrt.Needed() }

// uvEnv is the environment uv runs with: on a musl-path host, UV_LIBC names
// the libc uv cannot detect (no /bin/sh or loader to inspect).
func uvEnv(env []string) []string {
	if needsMusl() {
		// UV_PYTHON_INSTALL_BIN=0: no python3.x shim in ~/.local/bin (and no
		// PATH warning) — bashy resolves the managed interpreter itself.
		env = append(env, "UV_LIBC=musl", "UV_PYTHON_INSTALL_BIN=0")
	}
	return env
}

// EnsureMuslLoader installs musl's loader at /lib when this host needs it and
// it is missing. A no-op everywhere else.
func EnsureMuslLoader(ctx context.Context) error {
	return muslrt.EnsureLoader(ctx, "python", "bashy self image --with python")
}
