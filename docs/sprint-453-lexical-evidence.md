# Sprint 453 lexical retrieval evidence (story a566e6387674, story 2270)

Wave2 lexical delivery only. Semantic/hybrid (Wave4) return explicit
`unsupported`: no embeddings, no network, no silent substitution.

## What was delivered

Single holder commit on the final capture base `4f612295` (retrieval work
only: five new files, no capture edits, no generated `KB.md`):

- `pkg/dbfts/generation.go` — new `Generation(h)` export reusing the
  existing `index(h)` open plus the batch counter `generation(d.Index())`
  every `OpPut`/`OpDelete`/`OpBatch` commit bumps. No new counter, lock
  registry, or admission machinery (authorized mb:6799).
- `pkg/kbmemory/retrieval.go` — shared `Retrieve` read pipeline plus the
  explicit `SyncIndex` this story owns (manager correction mb:6684: the
  capture side owns no FTS target/config, so index maintenance lives here).
  Both verbs run their index side as registered category operations on the
  index target (`memory.retrieve`, `memory.index.sync`).
- `pkg/kbmemory/retrieval_cmd.go` — standalone Cobra (`retrieve`,
  `index-sync`) and MCP (`kb_memory_retrieve`, `kb_memory_index_sync`)
  constructors for a later Bashy mount; Bashy untouched here.
- `pkg/kbmemory/retrieval_test.go`, `retrieval_process_test.go` — short
  filtered in-process tests plus independent-process both-mode tests
  (skipped under `-short`; the manager runs them remotely).

## Seam (final correction steering 13:52 + mb6799/mb6776/mb6772/mb6764/mb6763/mb6753/mb6746/mb6716/mb6698/mb6684, receipt sent)

- Read seam: exactly two explicit targets — the sqlite memory collection
  (source of truth) and the bleve rank index — same stable record IDs in
  both. Recall never creates, writes, or provisions; a missing target is
  `not_found` naming it (`TestRetrieveNeverProvisions`).
- Source snapshot: ONE `doc.find{Collection:memory, Limit:10000,
  MaxBytes:8MiB}` read transaction. Any next cursor or truncated page
  refuses `limit_exceeded` before any index write — no unbounded export
  with caps checked after allocation, no temp files, no multi-page
  consistency claim (`TestSyncRefusesPartial`).
- Index side under ONE admission (mb6746 correction, no new lock
  registry): `memory.index.sync` (AccessWrite) takes the bounded source
  snapshot as input and, on the same admitted handle, runs the existing
  `fts.search` snapshot, diffs, and applies one atomic `fts.batch`
  (`Replace` + per-entry `IfRevision` on updates, enumerated deletes) via
  the existing category lookup/Run — never split across owner calls.
  `memory.retrieve` (AccessRead) brackets the bounded index snapshot and
  the bounded ranked query hits with two `dbfts.Generation` reads on the
  same admitted handle (final correction mb:6799/mb:6764: AccessRead does
  NOT exclude owner writers, and the same handle alone does not serialize
  readers with writers). A writer committing mid-read moves the batch
  counter — content ABA included — and the op returns `busy` with no
  output instead of a mixed-generation claim
  (`TestRetrieveBusyOnConcurrentIndexWrite`: a rendezvous holds the read
  inside the op while a separate admitted writer commits — first the
  normal sync path, then a true content ABA: `fts.put` plus `fts.delete`
  of one temporary indexed ID through the same owner admission, so the
  before/after index bytes are identical while the counter moved twice;
  equal snapshot fingerprints beside the `busy` prove only the generation
  tripped it, per review 14:02/mb:6839; the record-revise-away-and-back
  variant was dropped because updates also move `Updated`/revision and
  leave the extra record behind). The probe fires once per read
  (`sync.Once` guard), never writes through the read handle, and both the
  read goroutine and the inline writer join before the test returns — no
  hook or writer outlives the test (review 13:59). Sync (AccessWrite)
  stays serialized by the existing
  admission. The source snapshot beside the index admission is a separate
  explicit point: no cross-target atomicity is claimed.
- Best-N ranking: dbfts orders `_id`, so the retrieve op queries at the
  max bounded population and sorts by score descending (stable by record
  ID) before the requested limit (`TestRetrieveSortsByScoreBestN`).
- Refs (corrected mb:6763/mb:6772: `ParseTarget` rejects inner paths, so
  no provider descent is invented): `Hit.Ref` is the stable record ID
  verbatim; the canonical source target travels separately in the
  freshness report (`doc_target`/`index_target`) and the envelope target,
  so a record is named by the tuple (target, collection, ID).
  `Tombstone.Ref` is likewise the record ID (capture `4f612295`).
- Commands refuse inherited kb `--dir/--repo/--user/--base-dir/--ring`
  selectors exactly like capture, for retrieve AND sync
  (`TestRetrieveRefusesInheritedStoreFlags`); output write failures exit 1
  (`TestRetrieveWriterFailure`, reusing the capture writer).
- Freshness: `sha256` fingerprints over the bounded source and index
  snapshots (decoded records in stable form, same semantic comparison the
  sync uses), reported as `source_revision`/`index_revision`; `stale` is
  true exactly when they differ — including a same-count edit outside the
  query and a same-count delete-plus-add with all counts at 2/2
  (`TestRetrieveFreshnessFingerprints`). Recall serves every hit from the
  same source snapshot (no per-hit re-reads) and drops index entries the
  snapshot no longer holds (`dropped_stale`).
- Lifecycle at consumption through the one shared semantics (corrected
  mb:6772: status-only was interim and wrong for forgotten/future/expired
  records): `kblife.Visible(record.State(asOf))` — `Record.State` and
  `Validity` come from the capture M6 final this commit sits on, nothing
  is copied here. Default as-of is one fixed now per retrieval; `--as-of`
  takes an explicit RFC 3339 instant for a historical read (bad value is
  `invalid_input`). Forgotten/superseded records stay stored but are never
  served — even when the index still holds their pre-lifecycle bytes —
  counted as `dropped_state` (`TestRetrieveDropsSuperseded` via the real
  supersede verb, `TestRetrieveLifecycleForgetAsOf` for forget +
  historical visibility + read-mutates-nothing).
- Large shared-mode sync: a ~130KiB snapshot reconciles through a shared
  owner session, proving the inherited `MaxWireBytes` (64MiB, capture
  `4f612295`) replaced the old 64KiB owner request bound
  (`TestSyncLargeSharedSnapshot`).

## Acceptance mapping

| Umbrella clause | Evidence |
|---|---|
| Lexical recall, no embedding service | `TestRetrieveLexicalRoundTrip`; no model/embedding import on the path |
| Semantic/hybrid explicit | `TestRetrieveUnsupportedModes`: `unsupported` naming the missing backend; unknown mode `invalid_input` |
| Scope/tags/time filters | `TestRetrieveFiltersScopeTimeAndCancel`: tags-all, repos-any, OS-exact, since-inclusive/until-exclusive, inverted window rejected |
| Stable refs, scores, actual mode | `Hit{Ref: record ID, Score, Mode: lexical, Revision, IndexRevision}` asserted in round trip; canonical target in freshness/envelope tuple; score-desc best-N pinned |
| Consistent IDs, deletion propagation | Index IDs are record IDs; source delete → recall drops + counts `dropped_stale` → sync deletes (`TestSyncOwnsFreshness`) |
| Freshness incl. new unindexed docs | Fingerprint `stale` on any source/index divergence; 3-source/2-index gap reported stale before any hit differs |
| Same-count changes outside query | `TestRetrieveFreshnessFingerprints`: outside-query edit + delete-plus-add at equal 2/2 counts |
| Bounded results, cancellation | Single-snapshot caps refused, not truncated silently; limit/max-bytes truncation flagged; cancelled ctx → `cancelled` |
| Both modes, two processes | `TestRetrieveTwoProcessesPersistent`, `TestRetrieveTwoProcessesMemoryOwner`: parent captures, child syncs/recalls, sees the stale gap (remote gate; skip under `-short`) |
| CLI = MCP | `TestRetrieveCLIAndMCPParity`: byte-identical `--json` envelopes |
| No rank-as-truth | Per-hit `Stale` + fingerprint gap; never substituted modes |
| Lifecycle visibility | `TestRetrieveDropsSuperseded` (real supersede verb, stale index) + `TestRetrieveLifecycleForgetAsOf` (forget, historical `--as-of`, no mutation): hidden unless then-valid |
| Concurrent index write | `TestRetrieveBusyOnConcurrentIndexWrite`: mid-read commit → `busy` with no output; true content ABA (`fts.put`+`fts.delete` of a temp ID, equal index fingerprints) → `busy`; read serves again after |
| Large shared-mode sync | `TestSyncLargeSharedSnapshot`: >64KiB snapshot through a shared owner session |

## Honestly remaining

- Semantic/hybrid retrieval (Wave4): needs an explicitly configured
  embedding backend; this path only reports it missing (`unsupported`).
  No embeddings, no network on this path.
- `kb recall` / `kb search` and 284 M4 seeding off this pipeline, plus the
  Bashy mount of the new constructors: same holder, separate run. Cobra
  and MCP constructors are exposed (`NewRetrieveCmd`, `NewSyncIndexCmd`,
  `MCPRetrieveCommand`, `MCPSyncIndexCommand`); Bashy untouched here.
- Manager gates (not run here): remote whole-package run (incl. the two
  independent-process tests, skipped under `-short`) and live CLI/MCP
  smoke after review. Whole `dbfts` joins the owed manager gate with the
  new `Generation` export.
- Local gate (this session, Dragon): `go vet` clean on both packages;
  `go test -count=1 -short -run 'Retrieve|Sync|Lexical|Lifecycle'
  ./pkg/kbmemory/` green, `go test -count=1 -short ./pkg/dbfts/` green,
  `TestRetrieveBusyOnConcurrentIndexWrite` green `-count=5` and under
  `-race`. Every gate ran as a direct `go test` invocation whose exit
  status is the verdict — no `| tail` pipeline masking it (review 13:59).
