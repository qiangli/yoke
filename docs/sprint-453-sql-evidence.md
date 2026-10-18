# Sprint 453 SQL evidence — story af7f959dadcf / #2261

Worker: `s453-sql-opus` (registered first holder; author and committer),
weave yoke#361, branch `agent/weave-issue-361` on yoke main `1587fa4c`
(includes the accepted lifecycle producer `921053bf`). This candidate
supersedes `47ed0d28` (wrong identity) and drops the harness KB.md commit
`491bad09`; see "Manager review 23:58 disposition".
This is the one evidence record for the candidate. The manager's whole-package,
crossvet and remote independent-process CLI/MCP gates are outstanding; no
story acceptance is claimed from development-host tests.

## What was added

- `pkg/dbsql` — the SQL category over `pkg/dbstore/sqlitedb`: `sql.query`,
  `sql.exec`, `sql.schema.show`, `sql.schema.apply`, and the SQL logical
  interchange `sql.export` / `sql.import`. Each registers with dblifecycle.
- `pkg/dblifecycle/category.go` — the one seam: `RegisterCategoryOp` plus
  `Service.RunCategory`. A category operation declares its access (`read`,
  `write`, `exclusive`) from its typed input. The owner (`owner.go`) routes a
  registered op through the existing machinery unchanged: claim guard,
  per-target admission (exclusive for `exclusive` access), the mutation gate
  and request-id replay for non-read access, and the hold epoch check. A held
  target runs the op on the hold's own exclusive handle. `Request.Input`
  carries the typed input; `Response.Page`/`Warnings` are additive. No lease
  registry, no second locator parser.
- `cmds/db` — `db sql query|exec|schema show|schema apply TARGET` (typed MCP
  spells the last two `schema-show`/`schema-apply` as `subject`), and
  `db export TARGET --out DIR` / `db import TARGET --in DIR` dispatched to the
  target's category (`sqlite` → `sql.export`/`sql.import`; other engines
  return `unsupported`). New flags live in the shared `declareFlags`, so CLI
  help, `--explain`, `Schema()` and the typed `db_lifecycle` MCP tool all
  carry them: `--sql`, `--param [NAME=]TYPE:VALUE`, `--params JSON`,
  `--input FILE|-`, `--limit`, `--max-bytes`, `--since`, `--on-conflict`.
- `pkg/dbstore` — contract codes `confirmation_required`,
  `version_unsupported`, `malformed_input`, `limit_exceeded`; the SQLite
  adapter opens a read-only memory handle with `mode=ro` (the engine refuses
  writes, not only `query_only`).
- `pkg/atlas/packages.go` — census row for `pkg/dbsql` (no command row change;
  `db` already declares destroy/exec/persist).

## Acceptance map

| Requirement | Delivered | Evidence |
|---|---|---|
| query/exec/schema show/schema apply; creation only through `db create` | Every op opens an existing target through `Service.Open`/owner; missing target → `not_found`, nothing created. | `TestSQLVerbsBothModes` (query before create → `not_found`). |
| Typed binding, no interpolation | Values bind through `database/sql` args only. Params use contract §8.3 tags (`int` decimal string, `f64` number or `NaN`/`Inf`/`-Inf`, `bytes` b64, plain string = text, `null`); untagged numbers/booleans, out-of-range ints and unknown tags → `invalid_input`; `dec`/`ts`/`vec` → `unsupported` (no exact SQLite binding). Positional array or named object (`:n`, `@n`, `$n`, `n`). Validated before admission, so `--dry-run` rejects them too. | `TestTypedBindingAndResults` (2^53+1, int64 min, −Inf, empty blob, a quote-bearing text matches exactly one row), dry-run assertion in `TestSQLVerbsBothModes`. |
| Engine/connection-enforced read-only | A read opens `mode=ro` (file and memdb), sets `query_only`, and installs a commit hook that vetoes any write commit; `SQLITE_LIMIT_ATTACHED=0` on every op connection. A held (read-write exclusive) handle gets the same `query_only` + commit-hook enforcement for reads, and a read that changed any row (`total_changes()`, temp tables included) is refused. Every operation snapshots the connection pragmas `query_only`, `temp_store`, `locking_mode`, `foreign_keys`, `recursive_triggers`, `writable_schema`, `ignore_check_constraints`, `trusted_schema` at start and restores any that changed at the end (re-taking the native exclusive lock when `locking_mode` is restored); a connection still carrying temp objects or failing restore is discarded. Memory databases open with `temp_store=MEMORY` (no spill to temp files). | `TestReadPathRefusesMutation` both modes: insert, `pragma query_only=0; insert`, `begin; delete; commit`, temp table, `pragma user_version=7` (with and without toggling), DDL → all `read_only`; `vacuum into`, `attach` file and memdb → `unsupported`, no file written; data and `user_version` unchanged. `TestHeldHandleReadStillRefusesMutation`; `TestHeldReadLeavesNoConnectionState` both modes (toggle + TEMP create, `begin` + TEMP, `temp_store`/`foreign_keys`/`writable_schema` changes → `read_only` and no state left for the next held op; memory `temp_store=2`; `locking_mode=normal` through a read keeps the persistent hold's native lock); owner hold path in `TestIndependentClientsShareOwnerMemoryDatabase`. |
| Atomic transactions/scripts, rollback | `sql exec` (one `--sql` script or a `statements` batch) runs in one `BEGIN IMMEDIATE`; the commit hook admits only the operation's own final COMMIT, and the engine rollback hook (`RegisterRollbackHook`) fails the operation if any transaction rollback happened in between, so `ROLLBACK; BEGIN IMMEDIATE; …` cannot commit only the tail. Exec compares the stored main-schema definitions (type, name, tbl_name, rootpage, sql) before commit as well as the cookie, so a schema change is refused even if the caller rewrites `pragma schema_version`; temp objects left at the end are refused. Any error or cancellation rolls back. | `TestExecIsAtomic` both modes: failing 2nd statement, mid-script `commit`/`rollback`, `begin`, DDL, NOT NULL failure, `INSERT A; ROLLBACK; BEGIN IMMEDIATE; INSERT B` (script and batch), `CREATE`/`DROP`/`writable_schema` edit + `pragma schema_version=<prior>`, temp table, cancelled long insert → nothing applied. Both new checks were mutation-tested (disabling either makes the test fail). |
| Schema change / import replace: exclusive admission + confirmation | `schema apply` and `import --replace` declare `exclusive`: `--yes` or `confirmation_required`; direct path takes the whole-db claim then the native exclusive lock (claim released if the lock fails, nested claim reused); owner path admits exclusively and, under a hold, needs the current epoch. | `TestSchemaShowApplyAndBounds`, `TestInterchangeRoundTripsAcrossModes`, owner hold section of the process test. |
| Actionable SQL errors | Engine message preserved with the contract code: e.g. `backend_error: sqlite: SQL logic error: no such table: missing`, `conflict` for UNIQUE/PK/rowid, statement/record index in batch and import errors, `(rolled back; nothing was applied)`. | `TestTypedBindingAndResults`, `TestExecIsAtomic`. |
| Bounded typed results, cursors, cancellation, timeouts | `columns` (name + declared type or null) and tagged `rows`; `--limit` default 100 / max 10000, `--max-bytes` default 262144 / max 8 MiB; truncation sets `page.truncated` and a `bashy-cursor-v1` pagination cursor (`schema_version` discriminator, bound to target and a sha256 of op+SQL+params), resumed with `--since`; a single row over the byte bound → `limit_exceeded`. `--timeout`/cancel interrupt the statement (`timeout`/`cancelled`). | `TestSchemaShowApplyAndBounds`, cursor resume via CLI `--input -` in `TestSQLVerbsBothModes`, cancel cases in `TestExecIsAtomic`. |
| Both modes via independent CLI/MCP clients | Persistent: direct multi-process WAL. Memory: explicit owner session. | `TestIndependentClientsShareOwnerMemoryDatabase`: owner process + 11 concurrent client processes (2 writer agents × 5 batched execs + 1 reader) → 20 rows, no lost update; request-id replay applies nothing twice, reuse with other input → `conflict`; agent-b `claim_held` during agent-a's hold; agent-a without/with wrong epoch → `stale_epoch`; held read mutation → `read_only`; held `schema apply` + exec succeed; release; export from owner memory → direct persistent import; owner stop → `lifetime_ended`. `TestIndependentProcessesSharePersistentFileDirectly`: 3 processes × 25 ops (2 writers, 1 reader) → 50 rows. CLI owner via real `db session start` in `TestSQLVerbsBothModes`; typed MCP in `TestTypedMCPSQLQuery` (exec/query/read-only/schema-show; CLI and MCP envelopes byte-identical). |
| Logical interchange | `bashy-db-export-v1`, `format_version` 1.0: a new directory (refused if it exists) with `manifest.json` and one `records-NNNNNN.jsonl` per table, each with `records`, `bytes`, `sha256`, `columns` and the rowid alias. Manifest `schema_def` keeps every object's stored definition (tables with types, constraints, defaults, collations, generated columns, STRICT, WITHOUT ROWID; indexes incl. partial/unique; views; triggers), `user_version`, `application_id` and `sqlite_sequence`. Records are §8.3 values read with unary `+col`, so each value keeps its storage class (no DATE parsing). Snapshot = one read transaction (`snapshot.method: read-txn`, `consistent: true`); readers and WAL writers are not excluded. | `TestInterchangeRoundTripsAcrossModes`: persistent→persistent, memory→memory, memory→persistent, persistent→memory; the destination re-exports to identical schema and record files (sha256); int64 max, −2^53−1, 1e308, Inf, 0.1, empty blob, NULL, rowid gaps, DATE text, AUTOINCREMENT sequence, trigger recreated and working, generated column recomputed. `TestImportKeepsSequenceAboveDeletedHighID` (persistent→memory, memory→persistent): IDs 1–5, delete 4–5, round trip → one `sqlite_sequence` row, seq 5 above max 3, next generated ID 6. |
| Import validation, conflicts, atomic rollback | Whole input validated first (manifest shape, schema/format version, category/engine, object references, flat local paths, per-record strict decode, counts/bytes/sha256). Merge (default) needs identical definitions for every exported object, otherwise `conflict`; `--on-conflict error|skip|replace` uses upsert, so only uniqueness conflicts are skipped/updated (NOT NULL/CHECK/FK still fail). `--replace --yes` drops and recreates inside the same transaction; definitions that change rows or create undeclared objects are `malformed_input`; files re-verified inside the transaction. `defer_foreign_keys` checks FKs at commit. Everything is one transaction (atomic). | `TestImportMergeConflictPolicies`; `TestImportValidatesBeforeMutation` (malformed → `malformed_input`, major 2 and v9 → `version_unsupported`, checksum → `integrity`, untagged value → `invalid_input`, `../` path → `malformed_input`, missing manifest → `not_found`, smuggled `; create table` → `malformed_input`; target unchanged after each; export over an existing dir → `exists`); `TestImportCancellationRollsBack` (50k rows, cancelled replace → empty target unchanged). |

## Lifecycle fix found by this story

`db hold MEMORY_TARGET -- CMD`: the child's `bashy db` calls on the held memory
target presented epoch 0 (→ `stale_epoch`), because the hold-env fallback ran
only when no session was known and a memory target always names its session.
Persistent holds were unaffected. `cmds/db` now applies the hold env whenever
the target matches `BASHY_DB_HOLD_TARGET` and the session matches.
`TestMemoryHoldChildPresentsEpoch` covers it.

## Development-host execution record (Dragon, darwin/arm64)

Candidate `47ed0d28` run — HOST-POLICY DEVIATIONS, recorded as such and not
as acceptance evidence: a full `go build -o /tmp/yoke ./cmd/yoke` with a
real-binary smoke, and the independent-process tests
(`TestIndependentClientsShareOwnerMemoryDatabase`,
`TestIndependentProcessesSharePersistentFileDirectly`, and `cmds/db`
`TestSQLVerbsBothModes` / `TestMemoryHoldChildPresentsEpoch`, which spawn the
test binary as the `db` CLI and a real `db session start` owner) were
executed on Dragon. They passed there; the manager's remote gate is the
authority for them.

Rework run (this candidate) — focused unit tests only, no build of
`cmd/yoke`, no process tests:

```
go test -count=1 -skip 'TestIndependent' ./pkg/dbsql            ok  (9.1s)
go test -count=1 -run 'TestTypedMCPSQLQuery' ./cmds/db          ok  (in-process MCP)
go test -count=1 ./pkg/dbstore/sqlitedb                         ok
go vet ./pkg/dbsql ./pkg/dbstore/sqlitedb                       clean
```

Raw mutation evidence (fix disabled → regression fails):

```
dbsql_test.go:222: "insert into a values (3, 'x'); rollback; begin immediate; insert into a values (4, 'y')" applied
dbsql_test.go:231: rollback/begin batch = statement 3: conflict: constraint failed: UNIQUE constraint failed: a.id (1555) (rolled back; nothing was applied)
dbsql_test.go:246: "create table b(x); pragma schema_version=2" => <nil>
dbsql_test.go:246: "drop table scratch; pragma schema_version=2" => <nil>
dbsql_test.go:250: schema changed through exec: [["b:CREATE TABLE b(x)"]]
dbsql_test.go:231: held "pragma temp_store=1; pragma foreign_keys=1; pragma writable_schema=1; pragma query_only=0; insert into r values (2)" left connection state [[…"v":"1"…]] (was [[…"v":"0"…]])
```

Not run here (manager gates): whole touched-package suites, the
independent-process tests, `scripts/crossvet.sh`, cross-builds, remote host.

## Manager review 23:58 disposition

1. Identity: candidate recommitted with author and committer
   `s453-sql-opus`, one trailer set; harness commit `491bad09` (KB.md) dropped.
2. ROLLBACK/BEGIN atomicity: fixed with the engine rollback hook; regression
   in `TestExecIsAtomic` (script and batch forms).
3. Writable `schema_version`: exec compares stored schema definitions, not
   only the cookie; regression covers CREATE, DROP and a `writable_schema`
   edit each followed by `pragma schema_version=<prior>`.
4. Held read handles: pragma snapshot/restore, temp-object discard backstop,
   `total_changes()` refusal, memory `temp_store=MEMORY`;
   `TestHeldReadLeavesNoConnectionState`. Limit below.
5. `sqlite_sequence`: verified, `TestImportKeepsSequenceAboveDeletedHighID`.
6. Host-policy deviations recorded above.
7. KB.md not committed; raw failure output kept above.

## Remote smoke recipe (real binary, independent processes)

```sh
go build -o /tmp/yoke ./cmd/yoke && export BASHY_DB_SESSION_DIR=$PWD/s BASHY_COORD_DIR=$PWD/c
P="--engine sqlite --mode persistent --path p.db"
/tmp/yoke db create $P
/tmp/yoke db sql schema apply $P --yes --sql 'create table t(id integer primary key, v)'
/tmp/yoke db sql exec  $P --sql 'insert into t(v) values (?)' --param int:9007199254740993
/tmp/yoke db sql query $P --sql 'select v from t'                          # {"t":"int","v":"9007199254740993"}
/tmp/yoke db sql query $P --sql 'pragma query_only=0; insert into t(v) values (1)'   # read_only, exit 1
S=$(/tmp/yoke db session start --idle-ttl 5m | jq -r .result.id)
M="--engine sqlite --mode memory --session $S --name m"
/tmp/yoke db create $M && /tmp/yoke db sql schema apply $M --yes --sql 'create table t(id integer primary key, v)'
for i in 1 2 3 4; do BASHY_EPISODE=w$i /tmp/yoke db sql exec $M --sql 'insert into t(v) values (?)' --param int:$i --request-id r$i & done; wait
BASHY_EPISODE=h /tmp/yoke db hold $M -- sh -c "/tmp/yoke db sql exec $M --sql 'insert into t(v) values (5)'; \
  env -u BASHY_DB_HOLD_TARGET -u BASHY_DB_HOLD_SESSION -u BASHY_DB_HOLD_EPOCH BASHY_EPISODE=o /tmp/yoke db sql query $M --sql 'select 1' --wait 1s"
                                                                           # holder ok; other claim_held (exit 9)
/tmp/yoke db export $M --out m.exp && /tmp/yoke db import $P --in m.exp --replace --yes
/tmp/yoke db sql query $P --sql 'select count(*) from t'                   # 5
/tmp/yoke db session stop --session $S --yes; /tmp/yoke db sql query $M --sql 'select 1'   # lifetime_ended
```

MCP: the same verbs through the existing typed `db_lifecycle` tool, e.g.
`{"verb":"sql","subject":"query","target":"<S11 db: locator>","sql":"select 1"}`,
or generic `run_tool` with the CLI argv.

## Consumer bridge

No new MCP tool name and no new registration: Bashy already appends
`dbcmd.MCPCommand()`, whose schema now carries the new flags. Bashy needs only
a yoke pin bump to this candidate. go.mod/go.sum are unchanged.

## Honest limits

- Owner IPC bounds: one request ≤ 64 KiB (typed `--input` capped at 48 KiB) and
  ≤ 30 s per operation, so large memory imports/exports through an owner are
  bounded by that deadline. Direct persistent operations use `--timeout`.
- Writers through one owner serialize on its mutation gate (all targets);
  readers continue. Persistent direct writers serialize in SQLite (WAL,
  `busy_timeout` = `--wait`).
- `--request-id` deduplication exists only through an owner session; on the
  direct path it is refused with `unsupported` rather than silently ignored.
- SQLite exposes no revision: `revision` is omitted, cursors re-run the query
  at the next offset (warning says pages are not one snapshot). Export and
  `schema show` are single snapshots.
- Query results for columns declared DATE/DATETIME/TIMESTAMP whose TEXT the
  driver parses come back as `ts` values with a warning (select `+col` or
  `CAST(col AS TEXT)` for the stored text). Interchange is unaffected.
- Text that is not valid UTF-8 is refused (`unsupported`, hint `CAST(col AS
  BLOB)`), never replaced. SQLite stores an integral REAL compactly, so a bound
  −0.0 reads back as 0.0 (engine behaviour, identical in source and copy).
- ATTACH, VACUUM and VACUUM INTO are refused on every sql connection (target
  boundary). Virtual tables (FTS5, etc.) have no logical export in v1
  (`unsupported`); `sqlite_stat*` planner statistics are derived, not exported
  (warning: run ANALYZE). Interactive multi-call transactions are deferred by
  contract.
- Merge import fires the target's own triggers on inserted rows; schema
  objects are recreated in source order except triggers, which are recreated
  after rows load.
- If a held handle's connection cannot be restored after an operation it is
  discarded; for a persistent hold the native lock is re-taken on the next use,
  while the claim keeps every Bashy client out in between.
- No SQLite authorizer is exposed by modernc.org/sqlite v1.50.1, so a
  statement's own session PRAGMA takes effect for the rest of that one
  operation (e.g. an explicit `PRAGMA temp_store=FILE` inside a memory-mode
  query can let SQLite use unlinked temp files for that statement). It is
  restored when the operation ends and cannot persist or change the target.
  Pragmas outside the restored list (cache_size, busy_timeout, …) affect only
  performance/waits on that pooled connection.
- Exclusion is the lifecycle story's: host-local claim plus native lock or
  owner admission, never presented as distributed.

## Blocking review 7526: transient engine effects (continuation)

Candidate 7526ffb2 remains unaccepted. Restoring `temp_store` or
`locking_mode` after an operation cannot undo a temporary disk spill or prove
that an exclusive native lock stayed held. The focused regression
`go test ./pkg/dbsql -run '^TestHeldReadLeavesNoConnectionState$' -count=1`
on Dragon exits 1 at this candidate, with:

```
--- FAIL: TestHeldReadLeavesNoConnectionState (1.20s)
    --- FAIL: TestHeldReadLeavesNoConnectionState/persistent (1.12s)
        dbsql_test.go:253: held query admitted locking_mode=NORMAL
    --- FAIL: TestHeldReadLeavesNoConnectionState/memory (0.08s)
        dbsql_test.go:238: memory query admitted temp_store=FILE
FAIL github.com/qiangli/yoke/pkg/dbsql 2.840s
```

SQLite's supported `sqlite3_set_authorizer` callback runs at statement
preparation (and on reprepare), before a `SQLITE_PRAGMA` setting takes effect.
The narrow dependency option is a per-connection `RegisterAuthorizer` API in
the modernc driver, exposing action code, pragma name, and argument through
`database/sql.Conn.Raw`, with callback removal when the connection closes.
`dbsql` can then deny `temp_store` setters on memory sessions and
`locking_mode` setters while a persistent native exclusive hold is active.
Keep the callback installed through statement stepping and preserve the
existing commit/rollback hooks and cleanup checks. Pinned modernc v1.50.1
contains the engine entry point but no exported authorizer. No dependency pin
has been changed; manager coordinates the driver surface first.

SQLite's `SQLITE_TEMP_STORE=3` build option also forces temporary storage to
memory despite a PRAGMA override, but affects all consumers of that SQLite
build and does not protect held locking mode. The authorizer is the smallest
single supported mechanism for both named gaps. It does not itself prove that
all other potential file effects or lock paths are safe; focused adversarial
tests and the manager's remote gates remain required.

References: https://www.sqlite.org/c3ref/set_authorizer.html,
https://www.sqlite.org/compile.html#temp_store,
https://www.sqlite.org/pragma.html#pragma_locking_mode.

## Extension path for JSON / full-text / KV workers

Follow `pkg/dbsql`, not a new framework: one package per category that
`dblifecycle.RegisterCategoryOp("<cat>.<verb>", CategoryOp{Access, Spec, Run})`
at init, a typed input struct decoded strictly in `Access` (validate values
there so dry-run and owner admission refuse them), and `Run` on the
`dbstore.Handle` the lifecycle passes in (read access must enforce read-only
even on a held read-write handle). `cmds/db` adds the sub-noun verb parse next
to `sqlVerb` and reuses `categoryInput`; `db export|import` already dispatch to
`<category>.export|import` by the target engine's category, so registering
`kv.export`/`kv.import` (etc.) is all that is needed there. Reuse
`values.go`'s §8.3 tags and the `bashy-db-export-v1` manifest shape (category
`schema_def`, record files with sha256/records/bytes, `blobs` for large
payloads).
