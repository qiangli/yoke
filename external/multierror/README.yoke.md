# external/multierror — clean-room multierror (S488.3c)

Original yoke code, MIT-0-style permissive under the repository `LICENSE`.
Written clean-room for Sprint 488 story S488.3c: the MPL-2.0
`hashicorp/go-multierror` source was never opened; the design comes from
the call sites and documented expectations of its permissive callers only
(`go.podman.io/podman/v6` `pkg/errorhandling`, `pkg/bindings/images`,
`pkg/machine/shim` and `go.podman.io/common` `pkg/retry`,
`pkg/supplemented`, `libimage` — Apache-2.0). The public API is exactly
what those callers use:

- `type Error struct { Errors []error }` — the containers/common retry
  package type-switches on `*multierror.Error` and iterates `e.Errors`.
- `Append(err error, errs ...error) *Error` — nil-safe (typed-nil
  `*Error` starts a fresh aggregate), skips nil elements, flattens
  nested aggregates, and returns nil when the result would be empty so
  nil-checks and error-interface assignments behave.
- `(*Error).ErrorOrNil() error` — used by `errorhandling.JoinErrors`,
  `bindings/images`, `pkg/machine/shim` and `pkg/supplemented`.
- `(*Error).Error() string` — the `%d errors occurred:` header plus
  tab-bulleted list and trailing blank line, the shape the podman
  `errorhandling` comment describes (it `TrimSpace`s the result).
- `(*Error).Unwrap() []error` — standard multi-error unwrapping so
  `errors.Is`/`errors.As` traverse the aggregate.
- `(*Error).Len() int` and `Group` (`Go(func() error)`,
  `Wait() *Error`) — added in S488.3e for the Linux-only buildah
  `imagebuildah` caller (concurrent stage builds); `Wait` returns nil
  when every task succeeded.

## Why a local replace

The engine build reaches MPL code through
`external/podman/engine` → `pkg/oci` bindings → podman/containers-common
callers → `github.com/hashicorp/go-multierror` → `github.com/hashicorp/errwrap`.
Rewriting those upstream callers is out of scope, so the root `go.mod`
carries:

```
replace github.com/hashicorp/go-multierror => ./external/multierror
```

which keeps the import path while compiling this permissive code in.
Behavior is pinned by `pkg/mplfree` in the parent module (aggregation
contract test plus the engine-graph replacement check).

The nested module declares `github.com/qiangli/yoke/external/multierror`
so standalone consumers can fetch it at a yoke commit and use it on the
right side of their own
`replace github.com/hashicorp/go-multierror => ...` directive, the same
arrangement as `external/go-billy`.

## Re-syncing

Upstream is intentionally not vendored or tracked here. If a caller ever
needs more API surface, extend this file clean-room from that caller's
use (permissive sources only) and extend the `pkg/mplfree` contract
test alongside.
