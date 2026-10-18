# Sprint 453 time-series evidence — story 01b029625a5e / #2267

Worker: `muse-spark1.3` (registered first holder; author and committer).
Base: `d9714fd5` (fetched `/tmp/s453-memory-publish` `origin/main`, FF-only).
Phase 1 (constructor/schema seams): `pkg/dbts` only. No `cmds/db`, atlas,
caps, or shared-file writes; those wait for the manager-coordinated phase-2
mount. This is the one evidence record for the candidate. No story
acceptance is claimed from development-host tests.

## What was added

- `pkg/dbts/dbts.go` — package doc, op names, bounds, strict typed `Input`,
  series/label/timestamp/value/duration validation, the ts schema
  (`bashy_ts_series`, `bashy_ts_points`), the read/write `tx` helper,
  `bashy-cursor-v1` cursors bound to a sha256 of the logical request, and
  `Spec` plus dblifecycle registration for all eight ops.
- `pkg/dbts/ops.go` — `ts.ingest` (atomic batch, on_conflict
  error|skip|replace), `ts.range` (exact-series read with bounds and
  cursors), `ts.agg` (SQL time buckets, count/min/max/sum/avg subset),
  `ts.retain` (exclusive retention set/clear plus delete, wall-clock read
  inside Run only).
- `pkg/dbts/interchange.go` — `ts.export`/`ts.import` in
  `bashy-db-export-v1` (manifest with inline series defs, chunked
  `points-NNNNNN.jsonl` with sha256/records/bytes, single-read staged
  import, atomic single transaction, merge preserves target
  retention, replace restores exported retention). Each record file is
  read once into bounded private bytes (`maxImportFileBytes`, one file
  staged at a time — never the whole dataset, no spill of an
  in-memory database to disk) and those same bytes are checksummed,
  strictly decoded and applied; caller files cannot change between
  validation and apply, and any late failure rolls back so the target
  is unchanged.
- `pkg/dbts/remote.go` — explicit-endpoint VictoriaMetrics client
  (`NewClient`, no default/env fallback, credentials refused):
  `ts.remote.query` (`GET /api/v1/query_range`, one bounded page, no
  cursor) and `ts.remote.ingest` (`POST /api/v1/import` Prometheus text).
  The lifecycle target is the claim-guard anchor only; the endpoint in the
  input is the data-plane target. Endpoint identity
  `db:vm:remote:<endpoint>`.
- `pkg/dbts/dbts_test.go`, `pkg/dbts/remote_test.go` — short in-process
  tests, both modes throughout (httptest fixtures for remote, never a
  provisioned service). Numeric-type correction: tagged ints keep their
  tag and exact int64 bits via `value_kind`/`value_int` (REAL shadow
  for aggregates, old tables migrate on next write); only the remote
  float-text backend refuses beyond ±(2^53−1), in validation before
  any byte is sent.
- `pkg/dbts/process_test.go` — independent-process tests (owner + client
  re-exec): shared owner memory database, persistent direct sharing,
  request-id replay, contested stamps, hold/exclusive/epoch behavior,
  owner stop. They skip under `testing.Short()`; the manager batches
  process/live/remote gates.

## Acceptance map

| Requirement | Delivered | Evidence |
|---|---|---|
| SQLite timestamped datasets, typed ingestion/range/aggregation/explicit retention; SQL analytics, not full OLAP | Series datapoints in two package-managed tables over the sqlite engine; ingest/range/agg/retain ops; package doc states the analytics (not OLAP) label | `TestTSIngestRangeRoundTrip`, `TestTSAggBuckets`, `TestTSRetainDeletesOldPoints` |
| Reuse sqlitedb/dbsql lifecycle/admission; no second registry/lock manager | Ops register with dblifecycle; handles come from `Service.Open`/owner; claims/holds/epochs/replay reused unchanged | All tests run through `Service.RunCategory`; process tests through owner `Call` |
| Bounded VictoriaMetrics query/ingestion with cancellation and endpoint errors; never provision/start | Explicit endpoint per request, client timeout + ctx, structured `backend_error`, limit guards | `TestTSRemoteQueryAgainstFixture`, `TestTSRemoteEndpointErrors`, `TestTSRemoteIngestRendersPrometheusText` |
| Preserve existing OTel | `pkg/otelquery` untouched | `git status` shows only `pkg/dbts` + this doc |
| Both modes, import/export both directions, owner tests | Memory + persistent fixtures everywhere; four-direction round trips; owner process tests | `TestTSInterchangeRoundTripsAcrossModes`, process tests |
| Revision conflicts, bounded cancellation, destructive hold/epoch invariants | Duplicate stamp → `conflict`; cancelled ctx → `cancelled`; unconfirmed exclusive → `confirmation_required`; hold/stale-epoch paths | `TestTSIngestConflicts`, `TestTSCancelledContext`, retain confirmation, process hold section |
| Lossless JSON/JSONL both directions | §8.3 tagged ts/int/f64; re-export byte-identical; int64-ns stamps; tagged ints keep tag + full int64 range (dedicated `value_kind`/`value_int` columns, REAL shadow for aggregates) | Round-trip byte comparison, tagged-int assertions incl. int64 extremes |
| Staged import: same checked bytes, bounded, target unchanged on failure | One bounded read per file, checksum + strict decode of those bytes, single-transaction apply with rollback; `stageFile` takes `ctx` and checks it before staging and per record; strict one-value-per-line decode (second value/trailing garbage refused); `Lstat` regular-file gate refuses FIFOs/sockets/devices before open | `TestTSImportStagedBytesBound` (over-cap refused `limit_exceeded`, target unchanged), `TestTSImportNegatives` target-unchanged tail, `TestTSImportLateMalformedLeavesTargetUnchanged` (trailing-garbage later line refused `malformed_input`, pre-existing int64 extremes unchanged) |
| Approximate aggregates declared, never silent | min/max/sum/avg read the REAL shadow and warn when out-of-±(2^53−1) ints are in scope; count exact, in-range and count-only quiet | `TestTSAggApproximationDeclared` |

## Honest limits

- NaN is refused (`invalid_input`): the driver binds it as NULL, so
  storing it would be a silent loss (probed, then the probe removed).
  ±Inf round-trips. Tagged ints are stored exactly over the full int64
  range, including the extremes — never rounded, never coerced to f64.
  Only the VictoriaMetrics float-text backend refuses ints outside
  ±(2^53−1), in validation before any byte is sent (`invalid_input`).
  Tables made before the typed columns migrate on the next write
  (`ensureSchema` adds `value_kind`/`value_int`; earlier rows are f64
  by construction and read back unchanged — probed with a scratch
  old-schema database, probe kept out of the repo).
- Ingest/range/agg address one exact series (name plus full label set);
  retain addresses every series matching a name and label subset.
  Multi-series selectors are a later story, not this one.
- SQLite exposes no revision: `revision` is omitted and range/agg cursors
  re-run at the next offset (warning carried on truncation). Export is one
  read-transaction snapshot.
- Reads issue only fixed SELECTs with bound parameters, so a read cannot
  mutate even on a held read-write handle; there is no caller SQL text to
  enforce against.
- Remote reads are one bounded page (over-limit fails explicitly, the
  caller narrows the range); remote exclusion is cooperative only.
- Aggregates over out-of-±(2^53−1) ints are approximate by declaration
  (warning on the outcome), not by silence; refusing those aggregations
  was not chosen so in-range analytics keep working. Record files over
  64 MiB staged size are refused (`limit_exceeded`); larger datasets
  must arrive as more chunk files, which the exporter already writes.
- Phase-2 mount (manager-coordinated, not in this candidate): `cmds/db`
  `db ts` verbs, the atlas census row for `pkg/dbts` (its absence trips
  `TestCensusCoversEveryPackage` until the row lands), the sqlite caps
  `ts` category row, and whole-package/crossvet/remote gates. Remote
  httptest fixtures here are not live evidence.

## Development-host execution record (Dragon, darwin/arm64)

Focused short tests only. No full product build, no container, no ollama.

```
CGO_ENABLED=0 GOMAXPROCS=4 GOFLAGS=-p=2 go test -short -count=1 ./pkg/dbts/   ok (9.6s)
CGO_ENABLED=0 go vet ./pkg/dbts/   clean
gofmt -l pkg/dbts/   clean
```

19 top-level tests pass (both modes where applicable): the earlier
`TestTSTaggedIntExtremesRoundTrip` and four-direction interchange
assertions, `TestTSAggApproximationDeclared` (approximation warning
with int64 extremes in scope, count exact, count-only and in-range
quiet, out-of-scope quiet), `TestTSImportStagedBytesBound` (over-cap
manifest refused `limit_exceeded`, target unchanged), and the new
`TestTSImportLateMalformedLeavesTargetUnchanged` (trailing-garbage
later line refused `malformed_input`, pre-existing int64 extremes
unchanged); the two independent-process tests report SKIP under
`-short`. Staging reads each file once inside the import write
transaction (one file at a time, 64 MiB cap), so the write lock is
held during bounded staging I/O; any staged failure rolls back.
Direct `go test` exit status, no pipe masking. Not run here
(manager gates): whole-package suite, `scripts/crossvet.sh`,
independent-process tests, cross-builds, remote host.
