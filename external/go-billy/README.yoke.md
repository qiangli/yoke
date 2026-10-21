# external/go-billy — managed billy fork (S488.3a)

Upstream: `github.com/go-git/go-billy/v5` at `v5.9.0` (Apache-2.0, see
`LICENSE`). All files are byte-identical to upstream except:

- `osfs/os_bound.go`: the two `securejoin.SecureJoin` calls (`Chroot`,
  `abs`) now call `boundJoin`; the `filepath-securejoin` import is
  dropped. Marked with a modification notice per Apache-2.0 §4(b).
- `osfs/boundjoin.go` (new): clean-room `boundJoin` + `ErrPathEscape`.
  Written from POSIX path semantics and the observed BoundOS contract
  (`..`/absolute paths fold inside the base dir; symlink resolutions
  that would leave the base dir never yield a usable outside path;
  symlink loops are an error). The MPL-2.0 `filepath-securejoin`
  source was never read; only its public call signature as used by the
  permissive caller (`os_bound.go`) was consulted. No other file was
  changed.
- Test files (`*_test.go`) and the `test/` helper package were not
  vendored (they only exercise upstream CI); containment is pinned by
  `git/containment_test.go` in the parent module instead.

## Why a local replace

`yoke/git` reaches the MPL-2.0 `cyphar/filepath-securejoin` module two
ways: directly through `go-billy/v5/osfs`, and transitively because
`go-git/v5` itself calls `osfs.New`. Dropping our own `osfs` imports is
therefore not enough — `GOWORK=off go list -deps ./git/...` keeps
reporting cyphar. The root `go.mod` carries:

```
replace github.com/go-git/go-billy/v5 => ./external/go-billy
```

which removes securejoin (and its `cyphar.com/go-pathrs` dependency)
from the module graph while keeping the billy API — and the Git path
containment it provides — intact.

## Re-syncing

To re-sync with a newer upstream: copy the new version over this
directory (same file selection), re-apply the `os_bound.go` hunk
(import + two call sites) against the new `boundjoin.go`, and re-run
the story gates:

```
GOWORK=off go test -short ./git/...
GOWORK=off go list -deps ./git/...      # must not mention cyphar
GOWORK=off go list -m all               # must not mention cyphar
```

Note: `replace` directives do not propagate to downstream modules. Any
consumer that builds yoke as a library (bashy, outpost) needs the same
replace — or a published billy release without the securejoin import —
before its own graph is clean. Flagged for the S488.0 integration.
