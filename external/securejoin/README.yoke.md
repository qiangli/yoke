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
  symbol the engine callers reference (podman `pkg/util`
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

## pathrs-lite

Out of scope: the `pathrs-lite` subpackage is only reached from
Linux-only files (`libpod/container_internal_linux.go`,
`pkg/domain/infra/abi/play_linux.go`) and the Linux engine
dependency set (storage, selinux, runc). The host (`GOWORK=off`)
engine graph — the gate this story pins — imports only the root
`SecureJoin` package, so this module ships only that package. If a
future caller needs more API surface, extend this module clean-room
from that caller's use (permissive sources only) and extend the
`pkg/mplfree` contract test alongside.

## Re-syncing

Upstream is intentionally not vendored or tracked here. If a caller
ever needs more API surface, extend this file clean-room from that
caller's use (permissive sources only) and extend the `pkg/mplfree`
contract test alongside.
