# Sprint 453 — memory collection lifecycle/capture evidence (story 564cbba7c9fd)

Status: **candidate, not accepted.** Dragon ran short filtered unit tests only.
The manager runs the remote gates listed below.

## Scope delivered

`pkg/kbmemory` — `kb memory init|select|add|inspect|update TARGET [ID]`. It sits
beside `pkg/recall` and composes `pkg/dblifecycle` + `pkg/dbdoc` (db never
imports it; `pkg/kb` stays a leaf and supplies only vocabulary/provenance).
Persistent mode, memory mode (via its owner session), and persistent mode through
an explicit shared `--session` owner all take the same `Run` path. CLI and MCP
(`MCPCommand`) share one cobra parser, one `Run`, and one envelope.

**Not delivered:** supersede/forget. They depend on the 284 M6 lifecycle API,
which does not exist yet. The manager proposed it as a stdlib `pkg/kblife`
(Validity/Supersede/Tombstone/Resolve/Visible), and kbmemory will store its
fields on dbdoc and call it once the SHA is pushed. `kb memory supersede` returns `invalid_input`; nothing here competes
with M6 semantics.

## Review corrections (mb:6587/6609/6615/6618, conductor steering)

| Finding | Resolution | Test |
|---|---|---|
| Envelope used `schema` | single discriminator `schema_version` | all (`must`/`code` assert it); parity asserts `"schema_version":` |
| add/update replay: client generated ID/time (and a pre-read of the stored `updated`) before the owner hashed the request — after u1 (rev1→2) and u3 (rev2→3, later clock), retrying u1 hashed the newer time and returned `conflict` (mb:6655/6659/6663, REJECT 23702490) | kbmemory registers its own `dblifecycle.CategoryOp`s `memory.add`/`memory.update`. Their input is the **logical request** plus deliberate per-operation provenance (`source.tool/host/episode/ref`); the record ID and `created`/`updated` are generated **inside `Run`** on the admitted handle. The owner's existing replay digest therefore covers exactly the logical operation and returns the committed response — no client pre-read, no second registry. The record ID is random, minted inside `memory.add`; there is no persisted replay state (`request_digest`/derived IDs removed per mb:6670/6687/6694) | `TestRequestIDReplay` (u1, u3, retry u1 = original rev 2, no mutation), `TestRequestIDAcrossPrincipals`, `TestConcurrentSameRequest` (memory + persistent-shared), `TestIndependentProcesses` (remote) |
| replay scope across principals | declared: the owner keys a request ID by principal and digests the input with its provenance. Another principal/episode reusing a request ID is an independent request — an add writes its own record, a stale update is `conflict` and mutates nothing. Not claimed as replay | `TestRequestIDAcrossPrincipals` |
| owner `not_found` from a pre-read never matched (mb:6655) | the pre-read is gone; existence is checked on the handle inside `memory.add` | — |
| init replay repeated the collection create | `init --request-id` is refused `unsupported` (two mutations, not one replayable request) | `TestRequestIDReplay`, `TestNothingImplicit` |
| committed update + failed read-back looked retryable | ok envelope with `committed:true`, `readback_error` | (code path; remote fault injection if wanted) |
| inherited `--dir/--ring/--repo/--user/--base-dir` silently ignored | refused, exit 2, before any store opens | `TestCLIRefusalsAndCancellation` |
| stdin/file read outside the timeout | the timeout context is created first; the read is abandoned on ctx done → `timeout` 124 | `TestCLIRefusalsAndCancellation` |
| writer errors ignored | a failed JSON/text write exits 1 (notes a committed write) | `TestCLIRefusalsAndCancellation` |
| `NewMemoryCmd` used Background | derives `cmd.Context()`; MCP sets its ctx via `SetContext` → `cancelled` 130 | `TestCLIRefusalsAndCancellation` |
| help locators | help spells `ref.Locator` encoding (`db:sqlite%3Afile%3APATH`, `db:sqlite%3Amem%3ASESSION/NAME`), asserted against `ref.Locator.String()` | `TestCLIRefusalsAndCancellation` |

`TestPersistentLifecycle` FAIL was observed by the conductor on the killed run's
tree. On this resume against a2865302 it passed before any edit
(`ok … 3.971s`), so the failure did not reproduce here. The manager's remote
run should confirm it.

## Dragon results (short filtered, this commit)

```
go vet ./pkg/kbmemory                                   # clean
go test -short -count=1 -v ./pkg/kbmemory
--- PASS: TestPersistentLifecycle
--- PASS: TestMemoryModeLifecycleThroughOwner
--- PASS: TestPersistentThroughExplicitSharedOwner
--- PASS: TestNothingImplicit
--- PASS: TestConcurrentUpdatesLoseNothing
--- PASS: TestCLIAndMCPParity
--- PASS: TestRequestIDReplay
--- PASS: TestRequestIDAcrossPrincipals
--- PASS: TestConcurrentSameRequest
--- SKIP: TestChild                 (helper process)
--- SKIP: TestIndependentProcesses  (process-level: remote gate)
--- PASS: TestCLIRefusalsAndCancellation
ok  github.com/qiangli/yoke/pkg/kbmemory 8.286s
```

The first run of `TestConcurrentSameRequest` failed in the persistent subtest
with `conflict: request-id "c-add" reused with different arguments`. The test
had reused one request ID across two targets on one owner. The owner's replay
key is principal + request ID, not target, so the conflict was correct. The
test now uses per-subtest IDs.

## Remote gates (manager, claimed host)

```
go vet ./pkg/kbmemory ./pkg/atlas
go test -count=1 ./pkg/kbmemory ./pkg/atlas ./pkg/recall ./pkg/kb ./pkg/dbdoc ./pkg/dblifecycle
scripts/crossvet.sh
```

Process-level test, written but not run on Dragon (manager remote gate):

```
go test -count=1 -run 'TestIndependentProcesses' -v ./pkg/kbmemory
```

It re-execs the test binary as independent client processes with distinct
`BASHY_EPISODE`s against memory mode, persistent mode through a shared owner,
and direct persistent mode. It covers same-revision update contention (exactly
one wins, the other gets `conflict`/`busy`), a same-request-ID add from two
processes of one principal (one record, identical ID/created), a same-principal u1 retry after
another process's u3 (original rev-2 outcome, record stays rev 3), and the
direct path refusing `--request-id` (`unsupported`).

Still owed:
- the bashy real-path smoke after the mount patch below.

## Bashy patch scope (second same-holder run)

1. `internal/agentos/kbrecall.go` / `kb.go` dispatch — mount
   `kbmemory.NewMemoryCmd()` under `kb` beside recall. Its no-op
   `PersistentPreRunE` keeps kb's root hook from opening a legacy store.
2. `cobra_schema.go` `schemaKBCmd` — the same mount, so schema and runtime agree.
3. The dispatcher maps `*kbmemory.ExitError` to its `.Code` (2/9/124/130/1),
   so CLI exit codes match MCP `MCPCommand().Run`.
4. MCP registration: `kbmemory.MCPCommand()` beside the existing direct tools
   (`yoke/mcp/direct.go` pattern).
