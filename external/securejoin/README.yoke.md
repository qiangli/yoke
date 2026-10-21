# external/securejoin — clean-room SecureJoin (S488.3d)

Original yoke code, MIT permissive under the repository `LICENSE`.
Written clean-room for Sprint 488 story S488.3d: the MPL-2.0
`cyphar/filepath-securejoin` source — including vendor copies — was
never opened; the design comes from POSIX path semantics, the call
sites of its permissive callers, the one-line public signature from
`go doc`, and black-box behavior observed by executing the replaced
module. The public API is exactly what the shipped engine graph
uses:

- `SecureJoin(root, unsafePath string) (string, error)` — the only
  root-package symbol the engine callers reference (podman `pkg/util`
  dockerignore resolution, `pkg/lookup` passwd/group paths,
  containers/common `etchosts`, storage, buildah `parse`, cgroups).
  Absolute paths are treated as relative to root, `..` above root
  clamps inside, symlinks are expanded component-by-component with
  outside targets re-scoped under the root, and loops are an error.

## Why a local replace

The engine build reaches MPL code through
`external/podman/engine` → `pkg/oci` bindings → podman/storage/
containers-common/buildah/cgroups callers →
`github.com/cyphar/filepath-securejoin`. Rewriting those upstream
callers is out of scope, so the root `go.mod` carries:

```
replace github.com/cyphar/filepath-securejoin => ./external/securejoin
```

which keeps the import path while compiling this permissive code in.
Behavior is pinned by `pkg/mplfree` in the parent module (engine
graph replacement check plus the SecureJoin containment contract).

The nested module declares `github.com/qiangli/yoke/external/securejoin`
so standalone consumers can fetch it at a yoke commit and use it on the
right side of their own
`replace github.com/cyphar/filepath-securejoin => ...` directive,
the same arrangement as `external/go-billy` and
`external/multierror`.

## pathrs-lite and pathrs-lite/procfs

The Linux engine graph (`GOOS=linux go list -deps
./external/podman/engine`) also imports the `pathrs-lite` and
`pathrs-lite/procfs` subpackages. They are implemented here clean-room
from the permissive callers' use and the Linux openat2(2), openat(2),
proc(5) and statfs(2) manual pages — again without opening the MPL
source. The public API is exactly what those callers use:

- `pathrs.OpenInRoot(root, path)` / `pathrs.OpenatInRoot(rootFile,
  path)` — podman `libpod/container_internal_linux.go` and
  `pkg/domain/infra/abi/play_linux.go`, storage `userns.go` and
  `pkg/chunked`, runc `internal/pathrs`. Returns an O_PATH handle.
- `pathrs.Reopen(file, flags)` — every caller above plus
  opencontainers/selinux; upgrades the O_PATH handle through a verified
  procfs and checks the reopened file is the same inode.
- `pathrs.MkdirAllHandle(rootFile, path, mode)` — runc
  `internal/pathrs` `MkdirAllInRoot`.
- `procfs.OpenProcRoot()` / `OpenUnsafeProcRoot()`, `(*Handle).Close`,
  `OpenSelf(sub)`, `OpenPid(pid, sub)`, `OpenThreadSelf(sub)` returning
  a `ProcThreadSelfCloser` — selinux `go-selinux` and runc
  `internal/pathrs` / `libcontainer/apparmor`.

Confinement is file-descriptor based, never a lexical join followed by
an open: in-root lookups use openat2 `RESOLVE_IN_ROOT |
RESOLVE_NO_MAGICLINKS`; procfs lookups use `RESOLVE_BENEATH |
RESOLVE_NO_MAGICLINKS | RESOLVE_NO_XDEV` beneath a `/proc` handle
verified as a procfs root (`PROC_SUPER_MAGIC`, inode 1), and refuse
`..` in subpaths. Kernels without openat2 (pre-5.6) get an
openat(`O_NOFOLLOW`) walk that holds a handle for every directory on
the path and resolves `..` by popping back to the held parent; the
procfs walk re-checks each handle is procfs and refuses every symlink
except the leading `self`/`thread-self`. Off Linux the functions return
an "only supported on linux" error.

The two packages share their kernel helpers as identical copies
(`kernel_linux.go`, pinned by `TestKernelCopiesInSync`) instead of an
internal package: this module compiles under two module paths (as the
cyphar replacement and standalone), so an intra-module import cannot
resolve in both. The Linux behavioral tests (`*_linux_test.go`) need a
Linux host to run.

## Re-syncing

Upstream is intentionally not vendored or tracked here. If a caller
ever needs more API surface, extend this file clean-room from that
caller's use (permissive sources only) and extend the `pkg/mplfree`
contract test alongside.
