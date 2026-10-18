# Sprint 453 Object Category Evidence (Story e6da3d7c62e6 / #2268)

Category package `pkg/dbobj` implementing the Sprint 453 object storage category over SQLite and BBolt adapters (persistent) and `dbstore.MemSpace` (memory supplement). Operations register with `dblifecycle.RegisterCategoryOp`, so claims, owner admission, request-id replay and epoch fencing are the shared seam, unchanged.

## Surface

- Bucket operations:
  - `db obj bucket create|add TARGET --bucket BUCKET`
  - `db obj bucket drop|rm TARGET --bucket BUCKET [--yes]` (destructive, requires exclusive hold and confirmation)
  - `db obj bucket list TARGET`
- Object operations:
  - `db obj put TARGET --bucket BUCKET --key KEY [--content-type CT] [--checksum SHA256] [--if-revision REV] [--if-absent] [--metadata K=V...]` with text or base64 value
  - `db obj get TARGET --bucket BUCKET --key KEY` (returns metadata + encoded value)
  - `db obj stat TARGET --bucket BUCKET --key KEY` (returns metadata without blob fetch)
  - `db obj delete|rm TARGET --bucket BUCKET --key KEY [--if-revision REV]` (destructive)
  - `db obj list TARGET --bucket BUCKET [--prefix P] [--limit N] [--max-bytes B] [--since CURSOR]` (bounded listing with `bashy-cursor-v1` cursor pagination)
- Backup transfer:
  - `db obj transfer TARGET --bucket BUCKET [--destination DEST] [--remote REMOTE]` (delegates to read-only `yoke/external/rclone`, no service provisioning)
- Interchange:
  - `db obj export TARGET --out DIR` (logical export producing `manifest.json` under `bashy-db-export-v1` / format `1.0`, per-bucket `objects-XXX.jsonl` manifests, content-addressed `blobs/` directory, per-file record counts, bytes, and SHA-256)
  - `db obj import TARGET --in DIR [--on-conflict error|skip|replace] [--replace] [--yes]` (atomic validation of manifest, files, checksums, and blobs; `--replace` clears existing data within single transaction with confirmation)

## Acceptance Map -> Tests

| Requirement | Implementation and Evidence | Test |
|---|---|---|
| Explicit bucket lifecycle, memory and persistent modes | `OpBucketCreate`, `OpBucketDrop`, `OpBucketList` across SQLite and BBolt in both memory and persistent modes. Drop requires `--yes`. | `TestBucketLifecycle_MemoryAndPersistent` |
| Object CRUD, metadata, and preconditions | `OpPut`, `OpGet`, `OpStat`, `OpDelete` with MIME content-type, arbitrary key-value metadata, revision tracking, `if_revision` and `if_absent` CAS semantics. | `TestObjectCRUD_MetadataAndPreconditions`, `TestPreconditionChecks_IfRevisionAndIfAbsent` |
| Binary payload fidelity and checksum verification | Payloads encoded as valid UTF-8 text or base64. Client checksum verified on put; bad checksum rejected with `ErrInvalidInput`. Full binary round trip verified. | `TestBinaryPayloads_Base64`, `TestChecksumVerification_AndBinaryIntegrity` |
| Bounded listing, prefix filtering, and cursor pagination | `OpList` bounded by `--limit` (default 100, max 10000) and `--max-bytes` (default 256KB, max 32MB). Cursor uses standard `bashy-cursor-v1` format. | `TestBoundedListing_AndCursorPagination`, `TestMaxBytesTruncation` |
| Backup transfer with rclone | `OpTransfer` uses existing `yoke/external/rclone` client for local directories and remote endpoints without daemon/service provisioning. | `TestBackupTransfer_LocalAndRemote` |
| Lossless logical interchange across modes | `OpExport` and `OpImport` with `manifest.json` and `blobs/`. Tested memory <-> persistent in all combinations for SQLite and BBolt. | `TestInterchange_RoundTrips_MemoryAndPersistent` |
| Import pre-validation and atomic transaction | Validates all manifest fields, blobs, path safety, and records before ONE atomic transaction; bad later records leave target 100% unchanged. Supports `error`, `skip`, and `replace` conflict policies. | `TestInterchange_ManifestValidation`, `TestInterchange_ConflictPolicies`, `TestImport_BadLaterRecord_LeavesTargetUnchanged`, `TestImport_PathTraversalRefused` |
| Path confinement and traversal refusal | Safe path confinement (`safeJoin` and `os.OpenRoot`) prevents directory escape (`../`) and detects/refuses pre-existing destination symlink escapes before write. | `TestPathTraversalRefused_Transfer`, `TestTransfer_SymlinkEscapeRefused`, `TestImport_PathTraversalRefused` |
| Normalized key collision refusal and noncanonical key success | Bounded transfer preflights normalized key collisions (e.g. `a/b` vs `a//b`) refusing colliding pairs before write, while correctly transferring single noncanonical keys to normalized paths. | `TestTransfer_NormalizedKeyCollisionRefused`, `TestTransfer_NoncanonicalKeySuccess` |
| KV store atomic import and metadata preservation | Propagates all errors in KV transactions without ignoring `CreateBucket`/`DeleteBucket`/`Scan`; preserves existing bucket creation metadata on non-replace import. | `TestKV_ImportAtomic_MetadataPreserved` |
| Truncation detection in transfer and export | When `listObjects` returns `truncated == true`, partial transfer and partial export are refused with `ErrLimitExceeded`. | `TestTransfer_TruncatedRefused` |
| No implicit schema creation on read | Read-only operations (`bucket list`, `stat`, `get`, `list`) on clean/missing targets create no schema tables and return clean results or `not_found`. Schema created only on explicit mutations. | `TestNoImplicitSchemaOnRead` |
| Concurrent writes and cancellation | Exclusive holds and shared-writer serialization; cancellation checks before transaction commit. No process-global writer gates (admission + native transactions only). | `TestCancelledOperation_AndHoldSemantics`, `TestConcurrentWrites_NoLostUpdates` |
| Independent-process client testing | Multi-process test uses actual `os.Executable` subprocesses against the shared target. Skips `testing.Short()` per dragon guidelines. | `TestIndependentProcess_SkippedInShort` |

## Commands and Results on Dragon

- Focused test gate:
  ```sh
  CGO_ENABLED=0 GOMAXPROCS=2 GOFLAGS=-p=2 go test -short -timeout=2m -count=1 ./pkg/dbobj
  ```
  Result: `ok  github.com/qiangli/yoke/pkg/dbobj (PASS)`

- Multi-process integration gate (without `-short`):
  ```sh
  CGO_ENABLED=0 GOMAXPROCS=2 GOFLAGS=-p=2 go test -v -run '^TestIndependentProcess_SkippedInShort$' -count=1 ./pkg/dbobj
  ```
  Result: `PASS`

- Static analysis:
  ```sh
  go vet ./pkg/dbobj
  ```
  Result: Clean (exit 0)

## Remote Process Gate Recipe

To be executed on the manager's claimed remote test host:
```sh
go test -count=1 ./pkg/dbobj -run '^TestIndependentProcess'
```
