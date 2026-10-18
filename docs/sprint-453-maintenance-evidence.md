# Sprint 453 memory maintenance evidence (story b9d919218f1a, #2272)

Wave3 maintenance on the published capture+lexical base `6e34d7c2`. New files
only (`maintenance.go`, `maintenance_cmd.go`, `maintenance_test.go`,
`maintenance_process_test.go`); no capture/retrieval edits, no shared dispatch
or atlas rows. `NewMaintainCmd` / `MCPMaintainCommand` are unmounted
constructors for the later phase-2 mount.

## Seam

`Maintain(ctx, MaintainRequest) Envelope` (`bashy-kb-memory-maintenance-v1`,
errors reuse `ErrorSchema`). Verbs:

- `candidates` — read-only deterministic report: `expired` (kblife state),
  `stale` (still-valid candidate-status record updated before an explicit
  `--stale-before`), `duplicate` (same type + whitespace-collapsed body; oldest
  kept, reported as `of`). No autonomous forgetting, no model consolidation.
- `supersede` — the capture verb unchanged (one supersede semantics).
- `expire` — `memory.expire`: closes a valid record's interval at an instant
  taken inside owner execution; revision-guarded; `kblife.Validity.Check`.
- `delete` — `memory.delete`: revision-guarded dbdoc rm, requires `--yes`.
- `export` / `import` — dbdoc export/import (IDs, text, provenance travel
  unchanged). Export runs as `memory.export`: the memory-only check and the
  dbdoc export share ONE admitted read handle, so a collection created
  concurrently cannot leave through kb memory. Backup writes the native
  snapshot to a private sibling file, validates those exact bytes as
  memory-only, then publishes with a no-clobber link (a foreign database never
  appears under the requested name). Import first stages the bounded manifest and record file into a private
  directory, then validates and imports those same staged bytes (replacing the
  source afterwards changes nothing). It requires exactly the `memory`
  collection and validates every record (mem- id equals document id, form/type/body/timestamps) before
  the target is opened; `--replace` needs `--yes`.
- `backup` / `restore` — dblifecycle Backup/Restore (owner or direct).
  Restore needs `--yes`, copies the snapshot (bounded, cancellable) to a
  private file, validates that copy read-only (memory-only; every record
  well-formed; a truncated/continued `Outcome.Page` is refused, never ignored)
  and restores from the same copy, so the validated bytes are the replaced
  bytes; an invalid or foreign snapshot
  leaves the target untouched. If the post-restore verification fails the
  change is committed, so the result is an error envelope with
  `details.committed=true` and a `recover` instruction, never plain success.
  `--request-id` is refused (unsupported) for backup and restore, whose native
  operations have no replay record; other verbs forward it to the owner.

Mutations (and import/restore) accept `--index`; after the source commit the
explicit `SyncIndex` pipeline runs. If it fails the change stays committed and
the result carries `index_error` and a `recover` instruction (rerun
`index-sync`); retrieval keeps dropping hits the source no longer holds.

## Tests (both persistent and owner-held memory modes)

Short, in-process: candidates, expire revision/state, supersede with index,
delete propagation to index, interrupted-indexing recovery, index failure after
commit, export/import round trip, foreign-export refusal, target-safety refusals (foreign collection on export/backup, foreign snapshot on restore with the target unchanged, malformed-record and oversized-manifest imports, backup `--request-id`), backup/restore round
trip, bounded cancellation. Independent-process (`TestMaintainTwoProcesses*`,
skipped under `-short`): a child process deletes with index reconciliation; the
parent sees it in source and index.

Local results (CGO_ENABLED=0 GOMAXPROCS=4 GOFLAGS=-p=2, direct exit status):

- `go vet ./pkg/kbmemory` — exit 0
- `go test -short -count=1 -run Maintain ./pkg/kbmemory` — ok, exit 0
- `TestMaintainTwoProcesses*` — NOT run in this round (independent-process tests are remote-only per steering); last local pass predates the review fixes

The whole-package run before the final test-threshold fix had one failing
test (a `--stale-before` threshold in the test, not the code); it is fixed.
After the review fixes `go test -short -count=1 ./pkg/kbmemory` (whole package)
and `-run TestMaintain` both exit 0. The manager batches the whole-package, live and
remote gates; local fixtures here are not live evidence.
