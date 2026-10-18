# Sprint 453 full-text index evidence

Story #2264 (`8c8b6228daa2`). This candidate adds Bleve lexical operations behind the existing `db` command, typed `db_lifecycle` MCP tool, and `dblifecycle.RegisterCategoryOp`. It does not add vector search or a service provisioner.

## Acceptance map

| Requirement | Implementation and evidence |
| --- | --- |
| Explicit named lifecycle, persistent and memory | Existing `db create/show/drop` with `bleve` adapter; configured mapping passes through the same owner/direct create admission. `TestIndexOperationsBothModes`, `TestConfiguredMappingAndReopen`, `TestMemoryOwnerLifetime`. Memory is owner scoped and never spills to disk. |
| Lexical data operations | `db fts put/get/delete/batch/search/check/mapping show/rebuild`; source JSON is stored in the same atomic Bleve batch as the indexed document. `TestIndexOperationsBothModes`, `TestSearchFilterPaginationAndStaleCursor`. |
| Mapping | `db create --engine bleve ... --input FILE` accepts `{"mapping":{...}}`; mapping validates before creation and is fixed for that target. `fts.mapping.show` and export manifest expose it. Rebuild reindexes the existing mapping; a changed mapping requires a separately created target. |
| Sharing, conflict and exclusion | Category operations use existing claim/epoch/owner admission. Writers serialize before revision checks and batch commit. Updates require `replace:true` and `if_revision`; create conflicts on duplicate ID. Search cursors bind target, query and index generation. Export, import and rebuild acquire whole-index exclusive admission; imports and rebuild require `--yes`. `TestIndexOperationsBothModes`, `TestSearchFilterPaginationAndStaleCursor`. Independent-process gate is in `TestIndependentFTSClientsBothModes` for a remote host. |
| Lossless logical interchange | Export creates `manifest.json` (`bashy-fts-export-v1`, format version 1, mapping, count, SHA-256) and streams `documents.jsonl` in ID order. Source JSON preserves ID, numeric spelling and embedded base64 data; binary values must be encoded in JSON by the caller. Import validates version, mapping, unique IDs, source objects and checksum before one atomic Bleve batch. `TestLogicalRoundTripBothDirections`, `TestBatchAtomicityAndMalformedImport`, `TestFTSInputBoundsAndIntegrity`. Import/export are bounded to 100,000 documents, 128 MiB JSONL, 8 MiB per record and 1 MiB manifest. The atomic batch and validated input are retained in memory within these limits. Larger datasets require partitioning across explicit indexes. |
| CLI/MCP parity | `TestFTSRegisteredCLIAndTypedMCP` calls the actual registered `db` runner and typed `db_lifecycle` MCP tool and compares their search envelopes. |
| Cancellation and restart | Context is checked during search, export, import preflight and rebuild; canceled import leaves the target intact. Every persistent category call reopens the index; mapping and data survive reopen. `TestBatchAtomicityAndMalformedImport`, `TestConfiguredMappingAndReopen`. |

## Commands and results on Dragon

- `go test ./pkg/dbfts -run '^TestBleve' -count=1 -timeout 60s -v` — PASS after closing both `FieldDict` iterators. The prior hang log is preserved outside this patch.
- `go test ./pkg/dbfts ./cmds/db -run '^(TestBleveInternalStorage|TestBlevePersistentInternalStorage|TestIndexOperationsBothModes|TestLogicalRoundTripBothDirections|TestConfiguredMappingAndReopen|TestSearchFilterPaginationAndStaleCursor|TestBatchAtomicityAndMalformedImport|TestMemoryOwnerLifetime|TestFTSRegisteredCLIAndTypedMCP)$' -count=1 -timeout 60s -v` — PASS.
- `go test ./pkg/dbfts ./cmds/db -run '^(TestFTSInputBoundsAndIntegrity|TestConcurrentMemoryRevisionConflict|TestIndexOperationsBothModes|TestLogicalRoundTripBothDirections|TestFTSRegisteredCLIAndTypedMCP)$' -count=1 -timeout 60s` — PASS after review fixes. Oversize manifest/record and missing-source check fail before mutation; the unchanged target remains readable.
- `go build -o /tmp/yoke/yoke ./cmd/yoke` — PASS. Registered-path smoke: `db create`, `db fts put --input -`, then `db fts search --input -` against an explicit Bleve persistent path returned one hit and preserved integer `9007199254740993` in the result.

## Remote process gate recipe

Run on the manager's claimed remote test host, not Dragon:

```sh
go test ./cmds/db -run '^TestIndependentFTSClientsBothModes$' -count=1 -timeout 120s -v
```

That test launches a separately attached owner and independent CLI clients for memory and persistent targets, then checks concurrent writes and a separate search client. The manager's cross-process claim, crash/restart, CLI/MCP and whole-package gates remain required before acceptance. These were not run on Dragon.

## Limits

Bleve's mapping is immutable once an index is created. Rebuild uses the current mapping; migration to another mapping requires explicit creation and reingestion. The portable format is Bleve category v1, not a promise of arbitrary search-engine interchange. Search pagination is invalidated by any Bashy FTS write. External code that mutates Bleve internals without the Bashy source-data batch contract can leave source documents missing; `fts.check` reports that as integrity failure. The adapter's native persistent lock is host-local; distributed exclusion is not claimed. In-memory contents end with the named owner session. Engine work already inside a Bleve batch has no cancellable API; cancellation is checked before committing.
