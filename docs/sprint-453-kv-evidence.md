# Sprint 453 KV category evidence (story 10fbbb26510d / #2262)

Category package `pkg/dbkv` over the bbolt adapter (persistent) and `dbstore.MemKV`
(memory supplement). No new storage engine, no go.mod change. Operations register
with `dblifecycle.RegisterCategoryOp`, so claims, owner admission, request-id
replay and epoch fencing are the shared seam, unchanged.

## Surface

`db kv bucket create|drop|list|get|put|delete|scan|cas|batch TARGET` (typed MCP
spellings `bucket-create` etc.), plus `db export|import` which dispatch to
`kv.export` / `kv.import` from the target's category. Keys/values are
`{"text":...}` or `{"b64":...}` (flags `text:X` / `b64:X`). Buckets are one level,
UTF-8, 1..255 bytes. No bucket is ever created implicitly (put/delete/cas/batch on
a missing bucket is `not_found`); `bucket drop` and `import --replace` are
exclusive and need `--yes`.

TTL: not supported. bbolt caps notes say so; `--ttl` / `ttl` is refused as
`unsupported`. No Redis compatibility is implied.

Interchange: `bashy-db-export-v1` / format `1.0`, `manifest.json` + one JSONL file
per bucket (empty buckets included), lines `["b64 key","b64 value"]`, per-file
record count, bytes and sha256. Import validates manifest, versions, file names,
checksums and every record before any change, then applies in ONE transaction
(`--replace` empties first; `--on-conflict error|skip|replace` for differing
values; identical values are not conflicts so re-import is idempotent).
Export reads one read transaction (consistent snapshot), writes a new directory
only (existing path = `exists`) and removes it on failure/cancel.

## Limits (honest)

- Import/batch run in a single bbolt/memkv transaction: bounded by `MaxBatchOps`
  (1000) for batches; import size is bounded by memory of one transaction.
- Scan pages are separate snapshots; the cursor is the next key (warning emitted).
- Non-UTF-8 bucket names cannot be exported in v1 (`unsupported`).
- bbolt is single-writer-process: multi-process writers use an owner session
  (`dblifecycle` caps unchanged); memory mode lives only in the owner session.
- Real independent-process, MCP and whole-package/crossvet gates are the
  manager's (claimed remote hosts); not run here.

## Acceptance map -> tests

| Requirement | Test |
|---|---|
| explicit create/populate/get/update/delete/drop, both modes; no implicit bucket | `pkg/dbkv TestLifecycleGetPutDeleteBothModes` |
| persistent reopen | `TestPersistentReopenKeepsData` |
| binary/type fidelity, malformed input, TTL refusal | `TestBinaryAndInvalidInput` |
| bounded scans, prefix, cursor, max-bytes | `TestScanBoundsPrefixAndCursor` |
| CAS, same-key insert conflict, batch atomicity/rollback | `TestCASAndBatchAtomicity` |
| contested updates, no lost update | `TestConcurrentCASNoLostUpdate` |
| cancellation | `TestCancelledOperation`, `TestExportCancelledLeavesNothing` |
| round trips mem<->persistent all four directions | `TestInterchangeRoundTripAcrossModes` |
| conflict policies, replace, version/path/checksum/garbage refusal, no partial state | `TestImportConflictsReplaceAndValidation` |
| registered CLI path, exit codes, `--input`, export/import | `cmds/db TestKVVerbsPersistentCLI` |
| shared memory owner, request-id | `cmds/db TestIndependentKVSharedOwner` (spawns owner process) |

## Commands

```
go test ./pkg/dbkv
go test ./cmds/db -run KV
go test ./pkg/atlas ./pkg/dbstore/...
# remote-process recipe (manager): TestIndependentKVSharedOwner plus two shells:
#   yoke db session start; yoke db kv put --session S --engine bbolt --mode memory --name n ...
```

Integrated manager gates: see [shared wave 2 record](sprint-453-json-evidence.md#integrated-wave-2-manager-gate-2026-10-18).
