# Sprint 453 JSON document evidence — story d51543b72755 / #2263

Worker: `s453-json-opus` (registered holder; author and committer), weave
yoke#425, branch `agent/weave-issue-425` on yoke main `d661ccfc` (includes the
accepted SQL seam `bd29adf3`). This is the one evidence record for the
candidate. The manager's whole-package, crossvet and remote independent-process
CLI/MCP gates are outstanding; no story acceptance is claimed from
development-host tests.

## What was added

- `pkg/dbdoc` — the document category over `pkg/dbstore/sqlitedb`, registered
  with `dblifecycle.RegisterCategoryOp` (no second claim, lease, locator or
  owner path). Operations: `doc.get`, `doc.find`, `doc.put`, `doc.patch`,
  `doc.rm`, `doc.batch`, `doc.collection.create|drop|list`,
  `doc.index.add|rm|list`, `doc.export`, `doc.import`.
- Storage: a collection is a STRICT table `bashy_doc_c_<name>`
  (`id text primary key, revision integer > 0, doc text` with a
  `json_valid`/object CHECK). `bashy_doc_collections` and `bashy_doc_indexes`
  record which tables are collections and their index definitions. They are
  created by the first `collection create`, never earlier: no collection or
  metadata exists in a database until an agent asks for one.
- Documents: JSON objects decoded with numbers kept as their literal text and
  duplicate keys refused, re-encoded compactly without HTML escaping, stored
  as that text. Every write increments the document's revision. `put`
  generates a 32-hex ID when `--id` is omitted. `patch` is an RFC 7386 merge
  patch. `--if-revision` and `--if-absent` are preconditions. `batch` takes up
  to 1000 put/patch/rm ops in one transaction.
- Filters: a structured JSON tree (`and`/`or`/`not` and
  `{"field","op","value"|"param"}` with ops `eq ne lt lte gt gte in exists`).
  Values, including named `--params`, are always bound. Field paths must match
  `key(.key|[N])*` with keys `[A-Za-z_][A-Za-z0-9_]*`. Only then are they
  embedded as the literal JSON path, so an expression index matches the query.
  Comparisons also check the JSON type, so `1`, `"1"` and `true` never compare
  equal.
- Indexes: `index add --field PATH... [--unique]` creates a SQLite expression
  index over `json_extract(doc, '$.path')`. A unique index is enforced on later
  writes (`conflict`).
- Interchange: `db doc export TARGET --out DIR` writes `manifest.json`
  (`bashy-db-export-v1`, format `1.0`, category `documents`, `doc_format` `1`,
  collections and index definitions in `schema_def`, indexes listed as
  `derived`) plus one `records-NNNNNN.jsonl` per collection. Each line is
  `{"id","revision","doc"}` with the stored text. `db doc import TARGET --in
  DIR` validates the manifest and every record, including the sha256, count
  and byte length, before it changes anything. It then loads in one
  transaction and checks the checksum again inside it.
- `cmds/db/doc.go` — the `db doc …` verb split (`collection create` or typed
  MCP `collection-create`, and so on) and the typed input builder.
- Shared seams (manager told before the edits):
  - `cmds/db/db.go`: the `doc` dispatch, usage line, and flags `--collection
    --id --doc --patch --filter --if-revision --if-absent --index --field
    --unique` in the shared `declareFlags`. CLI help, `--explain`, `Schema()`
    and typed `db_lifecycle` MCP all carry them.
  - `pkg/dblifecycle/owner.go` `Call`: decodes owner responses with
    `UseNumber`. Without this, a document read through an owner session lost
    precision on the client (`12345678901234567890` became
    `12345678901234567000`, and `1.50` became `1.5`). The hold epoch reader in
    `cmds/db/db.go` and the four float64 epoch assertions in
    `pkg/dblifecycle/owner_test.go` and `pkg/dbsql/process_test.go` now read
    `json.Number`.
  - `pkg/dbstore/sqlitedb` caps: categories are now `["sql","documents"]`,
    with a note. `cats[0]` stays `sql`, so `db export|import` dispatch is
    unchanged.
  - `pkg/atlas/packages.go`: census row for `pkg/dbdoc`.
- No go.mod/go.sum change.

## Access, holds and transactions

| Operation | Access | Hold | Transaction |
|---|---|---|---|
| get, find, collection list, index list, export | read (read-only handle, `mode=ro`) | none | one read transaction (one snapshot per call) |
| put, patch, rm, batch, import (merge) | write | shared-writer | one `BEGIN IMMEDIATE` transaction; all or nothing |
| collection create, collection drop, index add, index rm, import `--replace` | exclusive (`--yes`; claim plus native lock, or the caller's held handle) | exclusive | one transaction |

Every schema change is exclusive, consistent with the accepted SQL decision
(`sql exec` refuses schema changes, and `schema apply` is exclusive). A held
target runs doc ops on the hold's own exclusive handle through the existing
owner path. Ordinary reads and writes are never serialized behind an
exclusive lease.

## Acceptance map

| Requirement | Delivered | Evidence |
|---|---|---|
| Collection create/drop, no pre-created collections | `doc.collection.create/drop/list`; metadata created only by the first create | `TestDocCRUDBothModes` (put before create gives `not_found`; a fresh list is empty; create needs `--yes`; second create gives `exists`; names validated), `TestDocCLIAndTypedMCPParity` |
| CRUD | get, put, patch, rm, batch | `TestDocCRUDBothModes`, `TestBatchIsAtomic` |
| Nested types and stable IDs | stored text keeps nested arrays/objects, big ints, `1.50`, `1.0e3`, `-0`, booleans, null and unicode exactly; generated IDs persist; export and import keep IDs and revisions | `TestDocCRUDBothModes` (exact stored text), `TestInterchangeRoundTripsAcrossModes` (full dump equality, including a non-ASCII ID with `/` and a space) |
| Nested-field and parameterized filtering | typed filter tree; values and `--params` bound; path grammar | `TestFilterIsTypedAndParameterized` (14 positive cases, 11 refused, including SQL-looking paths and values) |
| Indexes | expression and unique indexes; exclusive add/rm; unique enforced; planner uses the index | `TestIndexesAreExclusiveEnforcedAndUsed` (EXPLAIN QUERY PLAN names the index; a unique add over duplicates rolls back without leaving metadata) |
| Atomic updates | revision preconditions; merge patch; atomic batch | `TestBatchIsAtomic` (a third-op failure leaves ops 1-2 unapplied), `TestConcurrentRevisionUpdatesLoseNothing` (4 writers × 10 contested increments; final value 40, revision 41) |
| Bounded pagination | `--limit` (default 100, max 10000) and `--max-bytes` (default 256 KiB); keyset `bashy-cursor-v1` cursor bound to target+query; one oversized doc gives `limit_exceeded` | `TestFindPaginationIsBounded` |
| JSONL import/export, both directions | manifest + JSONL; merge with `--on-conflict error\|skip\|replace`, or `--replace` | `TestInterchangeRoundTripsAcrossModes` (persistent→persistent, memory→memory, memory→persistent, persistent→memory), `TestImportMergeConflictPolicies` |
| Malformed/unsupported versions, validation before mutation | major version, schema, category, doc_format, path, tamper, malformed JSON, non-object, duplicate key, revision, unknown field, missing newline, missing manifest, trailing JSON after a record (with the manifest count/bytes/sha256 recomputed, so only the parser can refuse it) | `TestImportValidatesBeforeMutation` (target unchanged after every refusal) |
| Revision exhaustion | a replaced document moves to max(current, imported)+1, strictly past both; at MaxInt64 import and put/patch give `limit_exceeded` instead of overflowing | `TestImportReplaceRevisionExhaustion` (replace over 5 gives 6; MaxInt64 rolls back, target unchanged) |
| Export fits its own import | a record line over `MaxRecordBytes` fails the export with `limit_exceeded` and removes the partial directory | `TestExportRejectsOversizedRecord` |
| Owner number fidelity | `Call` decodes with `UseNumber`; category results keep big-integer and decimal lexemes; hold epoch read as `json.Number` through acquire, nested refresh, stale release and release | `TestOwnerCallPreservesNumberLexemes` (in-process `Serve`, no subprocess; asserts `12345678901234567890`, `1.50`, `1E+2`, `-0.0`) |
| Cancellation | context checked while streaming; import is one transaction | `TestImportCancellationRollsBack` |
| Both modes, persistent reopen, memory lifetime | every package test runs in both modes; memory lives in the owner | `TestPersistentSurvivesReopenMemoryEndsWithOwner`; CLI smoke below (`session stop` gives `lifetime_ended`) |
| Exclusive destructive ops | exclusive ops refused under another native lock; held handle runs ops | `TestExclusiveOpsRespectNativeLock`, `TestReadHandleRefusesWrites` |
| CLI/MCP parity | typed `db_lifecycle` MCP subject spellings; same envelope | `TestDocCLIAndTypedMCPParity` (byte-equal envelopes for find, collection-list and index-list; exact number text through MCP) |
| Independent clients, both modes | direct persistent file; memory through an explicit owner session | `TestIndependentDocClientsBothModes` — **written for the manager's remote gate, not run on dev-host** (see below) |

## dev-host commands (named, in-process only)

```
go vet ./pkg/dbdoc ./cmds/db ./pkg/dblifecycle ./pkg/dbsql ./pkg/dbstore/sqlitedb   PASS
GOOS=windows go vet ./pkg/dbdoc ./cmds/db ./pkg/dblifecycle                            PASS
GOOS=linux go vet ./pkg/dbdoc ./cmds/db                                                PASS
go test -count=1 ./pkg/dbdoc                                                           PASS (15 tests; package has no subprocess tests)
go test -count=1 -run '^TestOwnerCallPreservesNumberLexemes$' ./pkg/dblifecycle        PASS
go test -count=1 -run '^(TestDocVerbParsing|TestDocCLIAndTypedMCPParity|TestHoldChildEnvUsesAcquiredHolderEpisode|TestTypedMCPSQLQuery|TestRegisteredLifecycleCommand|TestTypedMCPDbLifecycle)$' ./cmds/db   PASS
go test -count=1 -run '^(TestSQLiteLifecycleBackupRestore|TestBackupPublicationPreservesCompetingDestination|TestSQLiteMemoryNativeBackupRestore|TestBboltNativeBackupReopens|TestParseTargetUsesSharedLocator)$' ./pkg/dblifecycle   PASS
go test -count=1 -run '^(TestLifecycleBothModes|TestPersistentSurvivesReopenMemoryEndsWithLifetime|TestMemoryModeWritesNothingToDisk|TestCancellationAndInvalidOptions|TestClosedHandleRejectsUse)$' ./pkg/dbstore/sqlitedb   PASS
go test -count=1 ./pkg/atlas                                                           PASS
```

Registered-path smoke on dev-host (`go build -o /tmp/yoke425 ./cmd/yoke`, run
as `yoke db …`): `doc put` before create gave `not_found`. Create without
`--yes` gave `confirmation_required`; with `--yes` it succeeded. The rest
followed, in order:

- put and patch with `--if-revision`: ok; a stale revision gave `conflict`;
  `--if-absent` on an existing ID gave `exists`;
- typed filter and `--params` find: ok; a keyset cursor appeared at `--limit 1`;
- unique index add: ok; a duplicate put gave `conflict`;
- persistent export → import into a memory DB in an explicit `db session
  start` owner: merge gave `conflict`; `--replace --yes` succeeded;
- memory export → persistent `--on-conflict skip` import: ok;
- `db hold PERSISTENT -- yoke db doc get …`: ok;
- `session stop` → memory `doc get` gave `lifetime_ended`.

After the `UseNumber` fix, the owner-session get returns
`12345678901234567890` and `1.50` exactly.

Review follow-up (trailing JSON, revision exhaustion, export record size,
owner number regression): each new test was checked to fail with its fix
removed (trailing record accepted; MaxInt64 gave `backend_error`; oversized
export succeeded; without `UseNumber`, `1E+2` decoded as `100`). Search of
`dblifecycle.Call` consumers: the only numeric read is the `db hold` epoch
in `cmds/db/db.go`, which already parses `json.Number`; the `float64`
assertions elsewhere in the repo are not `Call` results.

Remote gate cce37efe (manager, Mac) reported two subprocess failures, both
in tests; production code is unchanged for them:

- `pkg/dbsql/process_test.go`: `epochOf` expects `json.Number`, but the
  client driver decoded the relayed owner response with plain
  `json.Unmarshal` (float64). The driver now decodes with `UseNumber`, as
  `Call` does. The other assertions marshal rows and are unaffected.
- `TestIndependentDocClientsBothModes/memory`: drop under hold gave
  `stale_epoch`. Diagnosis: the hold process and the drop client inherited
  one `BASHY_EPISODE`, so the owner saw the holder itself calling without its
  epoch and fenced it, which is designed behavior; fencing is not weakened.
  The hold process now has its own episode, so the drop must get
  `claim_held`/`busy`. Readiness polls a no-op exclusive probe (`index rm`
  of a missing index: `not_found` until held, then `claim_held`/`busy`)
  instead of a fixed sleep. A `find` before the hold and after the refused
  drop must match (data unchanged), the `db hold` exit is checked through
  `Wait`, and the drop after release must succeed.

This test also ran once on dev-host by accident (a `-run Doc` filter matched
it), with the same memory failure. It has not been run here since.

Not run on dev-host: `TestIndependentDocClientsBothModes` (after the fix), and the existing
subprocess tests whose epoch assertion changed
(`pkg/dblifecycle/owner_test.go`, `pkg/dbsql/process_test.go`). They compile
and vet.

## Manager remote gate (recipes)

```
go test -count=1 -run '^TestIndependentDocClientsBothModes$' ./cmds/db
go test -count=1 ./pkg/dbdoc ./cmds/db ./pkg/dblifecycle ./pkg/dbsql ./pkg/dbstore/...
scripts/crossvet.sh
```

`TestIndependentDocClientsBothModes` re-execs the test binary as independent
`db` CLI processes. It runs against a persistent file (direct) and against a
memory DB in an explicit owner session, checking:

- 3 clients × 4 contested `--if-revision` increments: no lost update, and only
  `conflict` or `busy` retries;
- concurrent same-ID `--if-absent` inserts: exactly one winner;
- `collection drop --yes` from another process while `db hold TARGET --
  sleep 3` is active: `claim_held` or `busy`, then success after release;
- after `session stop`, the memory database is gone.

Typed MCP over the stdio server is the same registered runner. The suggested
live check: the `db_lifecycle` tool with `verb=doc`,
`subject=collection-create|put|get|find|export`.

## Limits (recorded, not exempted)

- Object key order is not preserved: keys are stored sorted, since JSON objects
  are unordered. Values, types and number text are preserved; duplicate keys
  are refused rather than collapsed.
- Filters and indexes reach only keys matching `[A-Za-z_][A-Za-z0-9_]*` and
  array indexes. Other keys are stored and returned but cannot be filtered on
  in v1. Equality on whole objects or arrays is refused (`invalid_input`).
  `ne` and `not` include documents that lack the field. A filter integer
  outside int64 is refused. A stored integer beyond int64 is compared through
  SQLite's `json_extract` (as REAL), but it is never altered in storage or
  output.
- Pages are separate read transactions resumed by keyset on `id`, not one
  snapshot (stated in the result warning).
- Through an owner session a whole request is limited by the existing 64 KiB
  owner request bound, and `--input` by 48 KiB, so a larger document must use
  the direct persistent path (doc cap 4 MiB). `--request-id` deduplication is
  an owner-session guarantee only, as for SQL.
- Document tables are ordinary SQLite tables. `db sql query` can read them,
  and `db sql exec` can write them while bypassing revision increments (the
  CHECK constraint still keeps every doc a JSON object). `db export|import`
  of a sqlite target is the SQL interchange and carries them as tables; the
  document interchange is `db doc export|import`. `import --replace` replaces
  every document collection and leaves other SQL tables untouched.
- Merge import needs every exported collection to exist with identical index
  definitions; otherwise the result is `conflict` (use `--replace --yes`).
  Nothing is created implicitly.

## Integrated wave 2 manager gate (2026-10-18)

Shared gate record for KV, JSON documents and full-text candidates integrated on
`96986e52`, production candidate `adc453a2`; `2e5e300b` changes only the document
hold test to use a portable test-binary child instead of an external sleep program.
Original worker authors are preserved. No local module replacement is delivered.

- Remote macOS whole-package tests and vet passed for `pkg/dbkv`, `pkg/dbdoc`,
  `pkg/dbfts`, `pkg/dbsql`, `pkg/dblifecycle`, `cmds/db`, and `pkg/atlas`.
  This includes independent-process tests and the shared owner crash/restart,
  stale-epoch, alias exclusion and cancellation regressions.
- Repository crossvet passed Windows, Linux, Darwin and AIX lock canaries;
  registered applet test coverage passed.
- Native Bashy consumer built against this producer. Live FTS checks passed in
  both modes: three independent same-revision writers yield exactly one winner,
  cross-mode export/import retains the large integer, source check succeeds,
  and typed/generic MCP search results match.
- Native document CLI and typed/generic MCP passed in both modes, preserving
  integer 12345678901234567890 and cross-mode logical interchange.
- KV native Bashy checks passed on the earlier production-equivalent KV slice:
  independent CAS, binary keys/values, both transfer directions and CLI/MCP parity.
  Final integrated package tests cover the combined dispatch.
- Earlier Windows document hold failure and earlier SQL helper number-decoding
  failure are retained in raw gate logs. The portable hold child and UseNumber
  process helper fixes address these test defects; no exclusion assertion was removed.

Raw gate outputs and live JSONL receipts are retained by the manager. Windows
final candidate `2e5e300b` passed all seven packages and vet, including the portable
exclusive-hold test. Versioned consumer publication remains a separate gate.
