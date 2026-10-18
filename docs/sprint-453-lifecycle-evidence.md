# Sprint 453 lifecycle evidence — story d40c135c3627 / #2260

Candidate: worker `codex-gpt6-sol`, weave yoke#351. This is a **safe checkpoint**, not a delivery or acceptance claim. The same story and binding resume after the Sprint 446 S11 locator dependency publishes. No umbrella, bashy or manager changes are included here.

## Acceptance map and current evidence

| Contract row | Implemented checkpoint | Focused evidence | Remaining before acceptance |
|---|---|---|---|
| T1 lifecycle | Typed `dblifecycle.Service` over the existing adapters; registered `db` tool with explicit flag-form target, create/show/check/backup/restore/drop, register/unregister/list; SQLite quick check; no create on missing show/drop/restore. Restore requires existing target, `--replace` and `--yes`. | `TestRegisteredLifecycleCommand` passed before the coreutils schema call was added; `TestSQLiteLifecycleBackupRestore`, `TestSQLiteMemoryNativeBackupRestore`, `TestBboltNativeBackupReopens`, `TestBackupPublicationPreservesCompetingDestination` pass after current changes. | Re-run command dispatch after 446 S6 coreutils pin; S11 locator/alias wiring; real binary CLI/MCP smoke. Backend category stories own data verbs and logical interchange. Bleve/Bonsai native backup and bbolt native restore remain explicitly unsupported. |
| T2 lifetime and restart | Explicit owner holds `dbstore.MemSpace` in RAM; persistent bbolt reopens after owner crash; old memory session returns `lifetime_ended`. | `TestPersistentOwnerRestartAndMemoryOwnerLoss` passes with a real owner subprocess. | Remote Windows/native host run and full touched-package gate. |
| T9 owner sessions | Explicit session start/attach/list/show/stop CLI; Unix socket in 0700 directory with 0600 socket and bearer metadata; Windows named pipe with current-account/SYSTEM DACL and no metadata bearer token; bounded request reads/deadlines; 1024-result request-id replay window in owner RAM; two-minute attached-client handles, expiry, explicit detach, busy stop; no claim of exactly-once across owner failure or eviction. | `TestOwnerMemoryLifetimeAndReplay`, `TestOwnerAttachmentsAndBusyStop`, `TestExpiredAttachmentRejected`, `TestOwnerMutationAdmissionCancellation` pass. Tests use real independent owner/client test processes where stated. | Run actual multicall binary and MCP sessions on manager remote host; verify Windows named-pipe ACL and restart; inspect cancellation/timeout paths under native execution. |
| T10 exclusive use | Canonical `dbstore.Resolve` identity; existing `coord` kind `db`, claim epoch and backend exclusive adapter open; nested claim reuse; failure-safe release of newly acquired claim; owner checks presented epoch; stale release closes native handle without releasing successor claim; whole-database scope; no force. Per-target owner admission allows concurrent reads, serializes destructive work, and observes context/wait. | `TestOwnerHoldFencesOtherProcess`, `TestPersistentAliasCannotBypassOwnerHold`, `TestStaleOwnerHoldClosesBackendWithoutReleasingSuccessor` pass with independent processes/episodes and a path alias. | Direct CLI/MCP hold/release smoke and remote native gate; registration alias access after S11; more backend-specific busy/exclusion evidence belongs to each backend story. Bonsai read-only opens remain cooperative per adapter evidence, never claimed as native non-Bashy exclusion. |

## Development-host commands and raw outcomes

Dragon (darwin/arm64, Go from `go.mod`), focused tests only:

```text
go test -run TestSQLiteLifecycleBackupRestore -count=1 ./pkg/dblifecycle
  PASS (before owner changes)
go test -run TestRegisteredLifecycleCommand -count=1 ./cmds/db
  PASS (before tool.SchemaFromFlags call)
go test -run 'TestOwnerMemoryLifetimeAndReplay|TestOwnerHoldFencesOtherProcess|TestSQLiteLifecycleBackupRestore' -count=1 -timeout 30s ./pkg/dblifecycle
  PASS
go test -run 'TestPersistentOwnerRestartAndMemoryOwnerLoss|TestPersistentAliasCannotBypassOwnerHold' -count=1 -timeout 30s ./pkg/dblifecycle
  PASS
go test -run 'TestOwnerAttachmentsAndBusyStop|TestExpiredAttachmentRejected|TestPersistentOwnerRestartAndMemoryOwnerLoss|TestPersistentAliasCannotBypassOwnerHold' -count=1 -timeout 40s ./pkg/dblifecycle
  PASS
go test -run 'TestSQLiteMemoryNativeBackupRestore|TestBboltNativeBackupReopens' -count=1 -timeout 30s ./pkg/dblifecycle
  FAIL first: SQLite memdb VACUUM INTO wrote no filesystem image; corrected to modernc native backup API for memory
go test -run 'TestSQLiteMemoryNativeBackupRestore|TestBboltNativeBackupReopens|TestSQLiteLifecycleBackupRestore' -count=1 -timeout 25s ./pkg/dblifecycle
  PASS after correction
```

The current `cmds/db` build is **blocked** against yoke's pinned coreutils `cd33e2a` because it lacks `tool.SchemaFromFlags`; Sprint 446 S6 owns the pin update to coreutils `02b24ff1` and S11 owns `ref.ParseLocator`/`Locator.String`. No private parser/schema implementation or go.mod pin edit was made. The manager runs full touched packages, consumer checks and crossvet on a claimed remote host. This checkpoint has no such gate result.

## Known limitations and next action

- The current flag-form target works for typed lifecycle core, but S11 locator parsing and registered name/alias dereference are not wired. No default database, remote provider or implicit service is used.
- Domain `bashy-db-result-v1`/`bashy-db-error-v1` envelopes carry `schema_version`, `mutation_class` and `effects`; shared loom-v2 applies only where the common native wrapper owns output. Backend category stories own their data verbs and logical export/import.
- Owner attachment IDs expire after two minutes without use; one-shot operations without an attachment are transient. An owner loss invalidates its session and all RAM content. Dedup replay lasts 1024 successful mutations in one owner lifetime only.
- Persistent SQLite uses native WAL sharing; bbolt/Bleve/Bonsai require owner sharing where their adapter caps say so. The existing adapter limitations in `docs/storage-adapters-evidence.md` remain constraints. This checkpoint has no claim of remote/distributed exclusion.
- Next: commit checkpoint, yield worker351 so the same binding can run S11, then resume, rebase onto integrated yoke main, wire S11 locator/name resolution and updated coreutils schema, run command/MCP and remote evidence, finish this one story.
