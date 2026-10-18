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
  `points-NNNNNN.jsonl` with sha256/records/bytes, validate-first
  two-pass import, atomic single transaction, merge preserves target
  retention, replace restores exported retention).
- `pkg/dbts/remote.go` — explicit-endpoint VictoriaMetrics client
  (`NewClient`, no default/env fallback, credentials refused):
  `ts.remote.query` (`GET /api/v1/query_range`, one bounded page, no
  cursor) and `ts.remote.ingest` (`POST /api/v1/import` Prometheus text).
  The lifecycle target is the claim-guard anchor only; the endpoint in the
  input is the data-plane target. Endpoint identity
  `db:vm:remote:<endpoint>`.
- `pkg/dbts/dbts_test.go`, `pkg/dbts/remote_test.go` — short in-process
  tests, both modes throughout (httptest fixtures for remote, never a
  provisioned service).
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
| Lossless JSON/JSONL both directions | §8.3 tagged ts/f64; re-export byte-identical; int64-ns stamps, exact ints to ±(2^53−1) | Round-trip byte comparison, exact-int assertions |

## Honest limits

- NaN is refused (`invalid_input`): the driver binds it as NULL, so
  storing it would be a silent loss (probed, then the probe removed).
  ±Inf round-trips. Ints outside float-exact range are refused, never
  rounded.
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
- Phase-2 mount (manager-coordinated, not in this candidate): `cmds/db`
  `db ts` verbs, the atlas census row for `pkg/dbts` (its absence trips
  `TestCensusCoversEveryPackage` until the row lands), the sqlite caps
  `ts` category row, and whole-package/crossvet/remote gates. Remote
  httptest fixtures here are not live evidence.

## Development-host execution record (Dragon, darwin/arm64)

Focused short tests only. No full product build, no container, no ollama.

```
CGO_ENABLED=0 GOMAXPROCS=2 GOFLAGS=-p=2 go test -short -count=1 ./pkg/dbts/   ok (26.9s)
go vet ./pkg/dbts/   clean
gofmt -l pkg/dbts/   clean
```

15 top-level tests pass (both modes where applicable); the two
independent-process tests report SKIP under `-short`. Direct `go test`
exit status, no pipe masking. Not run here (manager gates):
whole-package suite, `scripts/crossvet.sh`, independent-process tests,
cross-builds, remote host.
