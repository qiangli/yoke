# Storage adapters — evidence (Sprint 453, story c381b87d6dfc / #2259)

`pkg/dbstore` is the CGO-free storage-adapter foundation behind the Sprint 453
database tools (contract: `dhnt/docs/sprint-453-database-memory-contract.md`).
This file records what was verified for the story's acceptance: engine reuse
without copies or cycles, exact pins and licenses, CGO-free paths, the vector
backend choice, the chromem-go correction, both storage modes, and the
limitations later stories inherit. It claims nothing about the later CLI, MCP,
lifecycle or interchange stories, and nothing about the manager's remote gates.

## Boundary

| Package | Imports | Serves |
|---|---|---|
| `pkg/dbstore` | stdlib only (`go list -deps` shows no non-standard package) | identity (`Resolve`, `Identity.ClaimUse`), `Mode`, `Options`, `Adapter`/`Handle`, error codes (§5.2), `Caps` (§7), `MemSpace` owner sessions, `MemKV`, `MemGraph`, `Guard`, `Gate`, `Retry` |
| `pkg/dbstore/sqlitedb` | `modernc.org/sqlite` | sql, both modes native |
| `pkg/dbstore/vecdb` | `sqlitedb` + `modernc.org/sqlite/vec` | vec (sqlite-vec `vec0`) on a sqlitedb database |
| `pkg/dbstore/bboltdb` | `go.etcd.io/bbolt` | kv; memory mode is the `memkv` supplement |
| `pkg/dbstore/blevedb` | `github.com/blevesearch/bleve/v2` | fts, both modes native (`NewMemOnly`) |
| `pkg/dbstore/bonsaidb` | `github.com/qiangli/bonsai/pkg/bonsai` | graph; memory mode is the `memgraph` supplement |
| `pkg/dbstore/dbstoretest` | `dbstore` | shared KV conformance suite |

Engines are the pinned upstream modules used in place; nothing is copied.
Each engine sits in its own subpackage, so its dependency tree reaches only
binaries that import it. The core never imports an engine, and no engine
package imports another except `vecdb → sqlitedb`, so there is no import cycle.
Nothing in yoke imports `pkg/dbstore` yet (atlas: `RoleUnwired`), so existing
consumers of these engines (ycode memex on Bleve, kg/ycode on Bonsai,
coreutils/yoke on bbolt and modernc SQLite) are not touched.

The `go.mod` change makes bleve, bleve_index_api, bonsai, dgo/v250 (used by the
bonsai test) and bbolt direct requirements. MVS raises a few indirect pins as a
side effect of bonsai's requirements: `chewxy/math32` v1.11.0→v1.11.1,
`google/flatbuffers` v24.3.25→v25.12.19, `go.yaml.in/yaml/v2` v2.4.3→v2.4.4.
Downstream consumers see these through MVS; the manager's remote gates are
where that is checked.

## Pins and licenses

Read from each module's license file in the module cache at the pinned version.

| Module | Pin | License |
|---|---|---|
| modernc.org/sqlite | v1.50.1 | BSD-3-Clause (`LICENSE`); bundled SQLite is public domain (`SQLITE-LICENSE`) |
| modernc.org/libc | v1.72.3 | BSD-3-Clause |
| sqlite-vec (in `modernc.org/sqlite/vec`, upstream v0.1.9 per modernc CHANGELOG) | v1.50.1 of modernc | upstream asg017/sqlite-vec v0.1.9 ships `LICENSE-MIT` and `LICENSE-APACHE` (dual MIT / Apache-2.0); the modernc `vec/` directory itself carries no separate license file, so attribution must name the upstream |
| go.etcd.io/bbolt | v1.4.3 | MIT |
| github.com/blevesearch/bleve/v2 | v2.5.7 | Apache-2.0 |
| github.com/blevesearch/bleve_index_api | v1.3.11 | Apache-2.0 |
| github.com/blevesearch/gtreap (Bleve memory store) | v0.1.1 | MIT |
| github.com/blevesearch/go-faiss | v1.0.26 | MIT — not linked (see CGO) |
| github.com/qiangli/bonsai | v0.0.0-20261013041512-fbfc95fa4dda | Apache-2.0 (`LICENSE.txt`) |
| github.com/dgraph-io/badger/v4 | v4.9.1 | Apache-2.0 |
| github.com/dgraph-io/dgo/v250 | v250.0.0 | Apache-2.0 (test-only use) |

All are permissive. No GPL, LGPL or MPL module enters `pkg/dbstore`.

The full transitive closure is in `docs/storage-adapters-licenses.tsv`
(module, pinned version, license from the module's license files). It has 103
modules and matches `go list -deps -test ./pkg/dbstore/...` exactly: 50
Apache-2.0, 25 MIT, 21 BSD-3-Clause, 4 BSD-2-Clause, 1 Apache-2.0+MIT, 1
BSD-3-Clause+MIT, and `modernc.org/memory` v1.11.0 as BSD-3-Clause+UNKNOWN. The
UNKNOWN there is `LICENSE-LOGO`, a Wikimedia link for the project logo, not a
code license. The module's code licenses (`LICENSE`, `LICENSE-GO`,
`LICENSE-MMAP-GO`) are all BSD-3-Clause.

## CGO-free paths

- `CGO_ENABLED=0 go build ./pkg/dbstore/...` succeeds (darwin/arm64, go1.27.2).
- `CGO_ENABLED=1 go list -deps -f '{{if .CgoFiles}}…' ./pkg/dbstore/...` lists
  only `runtime/cgo`, `plugin` (imported by `bonsai/tok`) and
  `prometheus/client_golang/prometheus`. Each has a non-cgo build that the
  `CGO_ENABLED=0` build above uses; none is required.
- `go-faiss` (cgo) is not in the dependency list: Bleve links it only under its
  `vectors` build tag, which nothing here sets.
- `modernc.org/sqlite/vec` is transpiled Go (`ccgo`); generated files exist for
  darwin, freebsd, linux (386, amd64, arm, arm64, loong64, ppc64le, riscv64,
  s390x), openbsd and windows (amd64, 386). Cross-platform compilation was not
  run on the development host by instruction; it belongs to the manager's
  remote crossvet gate.

## Vector backend choice

Evaluated first, as required: the pinned `modernc.org/sqlite` v1.50.1 already
contains a CGO-free sqlite-vec (`modernc.org/sqlite/vec`, added in v1.47.0,
upgraded to sqlite-vec v0.1.9). It is permissive (BSD-3 transpilation of a
MIT/Apache-2.0 upstream), adds no new module, and inherits SQLite's two modes,
WAL sharing and locks. `vecdb.TestVectorsBothModes` proves `vec_version()`,
`vec0` create, insert and exact KNN in both modes. It is selected.

Limitations: sqlite-vec is pre-v1, so its on-disk format may change between
upstream releases (logical export is the interchange path); KNN is exact
(brute force), there is no ANN index; importing `vecdb` registers `vec0` as an
auto-extension for every modernc connection in the process.

## chromem-go correction

The earlier all-permissive claim is wrong. `github.com/philippgille/chromem-go`
v0.7.0 (the pin in ycode's `go.mod`) is licensed **MPL-2.0** (module cache
`LICENSE` and upstream tag both read "Mozilla Public License Version 2.0").
MPL-2.0 is weak, file-level copyleft: usable, but not permissive, and it is not
used by this surface. It remains a ycode dependency only.

## Storage modes

| Engine | Persistent | Memory |
|---|---|---|
| SQLite | native file, WAL | native memdb VFS (`file:/<session>/<name>?vfs=memdb`), held by a pinned connection in the MemSpace |
| sqlite-vec | as SQLite | as SQLite |
| bbolt | native file | supplemental `memkv` (`dbstore.MemKV`): copy-on-write snapshots behind the same `dbstore.KV` interface |
| Bleve | native scorch directory | native `bleve.NewMemOnly` (upsidedown over gtreap) |
| Bonsai | native directory | supplemental `memgraph` (`dbstore.MemGraph`): typed nodes/edges with revisions and `Cond` preconditions, built on `MemKV`. Not DQL (operator-approved option b) |

No memory mode uses a temp file. `sqlitedb.TestMemoryModeWritesNothingToDisk`
and `blevedb.TestMemoryIndexWritesNothingAndEndsWithOwner` check the working
directory stays empty. Opening a missing target never creates it, in either
mode (`TestLifecycleBothModes` in each engine checks the path still does not
exist). A memory database lives in its owner's `MemSpace`; a closed space
answers `lifetime_ended`.

## Sharing, exclusion and capability metadata

Sharing and exclusion are reported per mode in `Caps.Modes[m].Sharing` and
`.Exclusion`. The engine-level `sharing` and `exclusion` (the §7 shape) are
derived by `dbstore.Summarize`, conservatively: native multi-process sharing
only if every mode has it, an owner required if any mode needs one, enforcement
points joined `owner+native-lock`, and `cooperative` if any mode is.

| Engine / mode | native_multiprocess | owner_required | enforced_by | non_bashy_clients |
|---|---|---|---|---|
| SQLite persistent | true | false | native-lock | blocked |
| bbolt persistent | false | true | native-lock | blocked |
| Bleve persistent | false | true | native-lock | blocked |
| Bonsai persistent | false | true | native-lock | cooperative (read-only opens bypass Badger's flock) |
| any memory mode | false | true | owner | blocked (no other process can reach it) |

Tests: `TestSummarizeDistinguishesModes`, and `TestCapsPerMode` /
caps assertions in each engine package.

## Transactions, admission and cancellation

- **bbolt / memkv.** `Update` is one serializable, atomic transaction; ctx is
  checked before fn and before commit, so cancellation rolls back. Writer
  admission goes through `dbstore.Gate`, a one-slot channel whose wait is
  bounded by `Options.Wait` and observes ctx. bbolt's own writer mutex is never
  contended by this handle's writers, so no writer waits on it unbounded. Other
  processes cannot reach that mutex (the writer flock excludes them). Writes
  through the raw `Bolt()` accessor bypass the gate. A commit that grows the
  file waits for read transactions in flight; keep reads short.
  Tests: `TestWriterAdmissionBoundedAndCancellable` (busy within the bound,
  cancelled ctx, readers not blocked, both modes),
  `TestMemKVWriterAdmissionIsBounded`, `TestGateAdmissionIsBoundedAndCancellable`,
  `dbstoretest.RunKVConformance` (no lost updates under 8 concurrent writers).
- **Bleve.** Each index write (`Index`, `Delete`, `Batch`, `SetInternal`,
  `DeleteInternal`) is admitted by a per-index `Gate`. A memory index shares
  one gate across all its handles. Bleve's API takes no ctx, so a write in
  progress cannot be cancelled. Searches take ctx (`SearchInContext`).
- **memgraph.** `UpdateWithin` uses the handle's wait; `Cond{IfAbsent, IfRev}`
  returns `conflict` on a contested write (`TestMemGraphContestedUpdate`).
- **SQLite.** Writers serialize in the engine, bounded by `busy_timeout` =
  `Options.Wait`. Statements take ctx; an interrupted statement is classified
  `timeout`/`cancelled` by `ClassifyCtx`.
- **Open/lock waits.** All open-time lock waits are bounded by `Options.Wait`
  and observe ctx via `dbstore.Retry` (bbolt, Bleve and Bonsai lock waits are
  not cancellable themselves, so each attempt waits one short slice).

## Closed handles

A memory database is shared by every handle in the owner. A handle that kept
working after `Close` could bypass a later exclusive `Acquire`. Every handle
now closes through `dbstore.Guard`. Close waits for operations in flight and
releases exactly once, and later operations fail with `lifetime_ended`:

- bbolt and Bonsai: `View`/`Update` (`TestClosedHandleRejectsUse`,
  `TestClosedMemoryHandleRejectsUse`); `Bonsai()` returns nil after Close.
- Bleve: every `Index()` the handle returned, including ones taken before
  Close, rejects reads, writes and `Advanced()`
  (`TestClosedHandleRejectsUseAndWritesAreGated`).
- SQLite: the handle's own `*sql.DB` pool is closed and `Classify` maps
  database/sql's closed-pool error to `lifetime_ended`
  (`sqlitedb.TestClosedHandleRejectsUse`).

Each test closes a handle, takes an exclusive open on the same target, and then
checks that the old handle is refused.

## Identity and claim coupling seams

- `dbstore.Resolve` gives the canonical identity: an absolute path with
  symlinks resolved and on-disk case, `db:<engine>:file:<path>`, or
  `db:<engine>:mem:<session>/<name>`. `TestResolveAliasesShareOneIdentity`
  covers symlinks, `..` and case aliases.
- `Identity.ClaimUse()` returns `("db", id.ID)`: resource kind `db`, match rule
  `name` on the canonical identity (§9.5). That is the seam for `bashy claim`.
  This package keeps no lease registry and parses no locator; 446 owns the
  locator parser.
- `Options.Exclusive` is the native half of a hold: bbolt's writer flock,
  SQLite `locking_mode=EXCLUSIVE`, Bleve's root.bolt flock, Bonsai's Badger
  flock plus its process slot, or an exclusive `MemSpace.Acquire`. Claim first,
  then backend lock, with release on failure, is the lifecycle story's job.

## Known limitations (inherited by later stories)

- Bonsai: one open database per process (process-global engine state); a second
  open fails `busy`. Read-only opens are not excluded by a writer. Index
  rebuilds use `os.TempDir` (persistent mode only).
- Bleve: scorch accepts writes on a read-only open without persisting them, so
  read-only is enforced by the handle wrapper. Persistent drop releases the
  flock before `RemoveAll`; the lifecycle `db hold` must cover that window.
- SQLite: an idle reader holds no file lock, so persistent drop relies on the
  lifecycle hold. On Windows, drop releases the lock before removal.
- bbolt: on Windows, drop releases the flock before removal.
- Multi-process sharing for single-owner engines and memory databases needs the
  owner session (`MemSpace` is in-process only). The owner and attach transport
  is the lifecycle story, not this one.

## Verification on the development host

Short, focused runs on darwin/arm64 (go1.27.2), one package at a time, as
instructed. There were no cross-compiles and no whole-repository suites:

```
go test -count=1 ./pkg/dbstore                 ok
go test -count=1 ./pkg/dbstore/bboltdb         ok
go test -count=1 ./pkg/dbstore/blevedb         ok
go test -count=1 ./pkg/dbstore/bonsaidb        ok
go test -count=1 ./pkg/dbstore/sqlitedb        ok
go test -count=1 ./pkg/dbstore/vecdb           ok
go test -count=1 ./pkg/dbstore/dbstoretest     ok
CGO_ENABLED=0 go build ./pkg/dbstore/...       ok
```

Not run here, and left to the manager's remote gates: full touched-package
gates under the CI ratchet, `scripts/crossvet.sh` (cross-OS), the
`pkg/atlas` census, and any downstream consumer build.
