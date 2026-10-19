# Sprint 453 lifecycle evidence — story d40c135c3627 / #2260

Worker: `codex-gpt6-sol`, weave yoke#351. The delivery branch is rebased onto
yoke main `2111a999` (Sprint 446 S6/S11). This is the one shared evidence
record for the candidate. Manager remote gates and the sequential Bashy
typed-MCP bridge/pin are still outstanding; no story acceptance is claimed
from development-host tests.

## Acceptance map

| Row | Delivered path | Evidence and limit |
|---|---|---|
| T1 lifecycle | `pkg/dblifecycle.Service` and registered `db` command implement explicit create, register/unregister, list, show, check, backup, restore and drop. The target can be flags, an S11-encoded `db:` locator or a registered resource name/alias; `dbstore.Resolve` canonicalizes the claim identity. Missing show/drop/restore do not create data; restore requires an existing target plus `--replace --yes`. SQLite check is `quick_check`; other engines report `open_only`. | Focused command dispatcher, SQLite native round trip, memory SQLite and bbolt snapshot tests below. Backend category stories own data verbs and logical export/import. Native backup works for SQLite in both modes and persistent bbolt. Native restore works for SQLite in both modes. bbolt memory native backup, bbolt native restore and Bleve/Bonsai native backup/restore return `unsupported` with a reason. |
| T2 lifetime | Memory contents stay in owner `MemSpace`, never a temp-disk substitute; persistent bbolt reopens after owner process crash, while old memory session calls report `lifetime_ended`. | Existing `TestPersistentOwnerRestartAndMemoryOwnerLoss` uses independent processes; rerun on the claimed remote host. No one-shot CLI claims persistent memory. |
| T9 sessions | Local authenticated IPC: Unix socket in 0700 directory, 0600 socket and token metadata; Windows named pipe DACL for current account and SYSTEM, no metadata token. Explicit start/attach/list/show/stop, attached-client presence/TTL and busy stop; request deadlines/cancellation, bounded mutation gate and 1024 successful-mutation replay entries in owner RAM. Start retains child ownership until ready; forced stop drains active request handlers before closing holds/memory. | `TestOwnerSessionOperationMetadata` and cancellation unit pass locally. Existing independent owner/client tests require remote rerun. Manager's Windows smoke on old checkpoint `114b6238` proved start → memory create/check → stop → old show `lifetime_ended`, and found stop metadata wrong; this candidate fixes the metadata and adds an actual stop-envelope assertion. Replay is only within one live owner and its bounded window; failures, eviction and owner loss do not provide exactly-once execution. |
| T10 holds | Existing `coord` db resource kind and epoch claim; canonical path/alias identity; nested caller claim reuse; backend exclusive open and failure-safe claim release; owner checks epoch on access. Stale holder cleanup frees its native handle and map entry without releasing a successor claim. Owner admission is per target, with readers sharing; global hold map lock is not held over backend acquisition. `db hold TARGET -- CMD` starts or uses an explicit owner, passes session/epoch to the child and releases at child exit; plain hold returns session/epoch for explicit release. | Local claim/admission unit evidence and independent process tests below. Actual Bashy one-command hold and typed MCP require remote smoke. Adapter limitations remain: Bonsai persistent read-only opens are cooperative, so no non-Bashy native exclusion is claimed. No force takeover or distributed-lock claim. |

The command emits `bashy-db-result-v1` and `bashy-db-error-v1` domain envelopes
with `schema_version`, `mutation_class` and `effects[]`, preserving the 446
domain-exit exception. `tool.PreviewLevel` is `local-preview`; `--explain`
reports `explain-only`. Both previews leave a missing path absent. The
single `declareFlags` source drives CLI parsing/help, `Schema()`/explain and
the typed `MCPCommand()` projection. Generic MCP `run_tool` also invokes the
same registered command. The typed direct tool is `db_lifecycle`; Bashy must
append `dbcmd.MCPCommand()` to its existing registered MCP adapters while
preserving generic `db` selection and policy.

Atlas classifies registered `db` as a storage tool with maximum effects
`read,write,destroy,exec,persist`, irreversible and experimental. `destroy`
reflects drop/restore, `exec` reflects owner start and `hold -- CMD`, and
`persist` reflects persistent targets. MCP policy therefore requires an
explicit `destroy` grant even for a read-only invocation of this umbrella
tool; the parity tests grant it. The policy is coarse at command level and no
per-operation bypass is claimed.
Its origin is `bashy` (the lifecycle tool is native, not a historical Unix
utility); `TestOriginCountsPinned` accounts for this addition.

## Focused development-host results

dev-host, darwin/arm64, Go from `go.mod`; tests were short and native. Exact
post-rebase commands run on this candidate's working tree:

```text
go test -count=1 -v -run TestRegisteredLifecycleCommand ./cmds/db
  PASS; raw JSON/exits are logged by the test. Key sequence:
  missing show 1/not_found, create 0, check 0/quick_check,
  S11 encoded-locator show 0, backup 0,
  restore missing 1/not_found (destination absent),
  explicit create + restore --replace --yes 0,
  unconfirmed drop 2/invalid_input, confirmed drop 0.
go test -count=1 -run 'TestRegisteredLifecycleCommand|TestMCPRunToolLifecycleParity|TestTypedMCPDbLifecycle' ./cmds/db
  PASS; in-memory MCP generic run_tool and typed db_lifecycle discovery/calls.
go test -count=1 -run 'TestParseTargetUsesSharedLocator|TestSQLiteLifecycleBackupRestore|TestSQLiteMemoryNativeBackupRestore|TestBboltNativeBackupReopens|TestBackupPublicationPreservesCompetingDestination|TestOwnerMutationAdmissionCancellation|TestExpiredAttachmentRejected' ./pkg/dblifecycle
  PASS.
go test -count=1 -run 'TestOwnerSessionOperationMetadata|TestParseTargetUsesSharedLocator|TestSQLiteLifecycleBackupRestore|TestOwnerMutationAdmissionCancellation' ./pkg/dblifecycle
  PASS after stop metadata correction.
go test -count=1 -run 'TestToolTableMatchesRegistry|TestClosedVocabularies|TestReversibilityCoverageAndConsistency|TestStabilityCoverageOnVerbs' ./pkg/atlas
  PASS after adding the registered tool row and effects/classes.
go test -count=1 -run 'TestRegisteredLifecycleCommand|TestMCPRunToolLifecycleParity|TestTypedMCPDbLifecycle' ./cmds/db
  PASS with an explicit MCP destroy grant; the typed denial case asserts policy
  exit 126 without it, even for a read invocation of the umbrella command.
go test -count=1 -run 'TestOwnerSessionOperationMetadata|TestSQLiteLifecycleBackupRestore|TestBackupPublicationPreservesCompetingDestination|TestOwnerMutationAdmissionCancellation|TestExpiredAttachmentRejected' ./pkg/dblifecycle
  PASS.
go test -count=1 ./pkg/atlas
  PASS after correcting db origin; all atlas package tests, including pinned
  origin counts, pass on dev-host.
go test -count=1 -run 'TestRegisteredLifecycleCommand|TestMCPRunToolLifecycleParity|TestTypedMCPDbLifecycle' ./cmds/db
  PASS on the origin-corrected candidate.
go test -count=1 -run 'TestOwnerSessionOperationMetadata|TestSQLiteLifecycleBackupRestore|TestBackupPublicationPreservesCompetingDestination|TestOwnerMutationAdmissionCancellation|TestExpiredAttachmentRejected' ./pkg/dblifecycle
  PASS on the origin-corrected candidate.
```

An earlier `go test -count=1 ./pkg/dblifecycle` passed in 2.437s on dev-host
before these latest edits. It included independent process tests and is not
used as the required remote acceptance gate. The manager owns the final
touched-package, consumer and crossvet runs on a claimed remote host.

## Remote acceptance still owed

- Build the candidate yoke/Bashy with the final yoke pin, then exercise actual
  `bashy db` dispatch and stdio MCP `run_tool` plus typed `db_lifecycle`.
- Rerun independent-process owner tests on a remote native host:
  `TestOwnerMemoryLifetimeAndReplay`, `TestOwnerAttachmentsAndBusyStop`,
  `TestOwnerHoldFencesOtherProcess`, `TestStaleOwnerHoldClosesBackendWithoutReleasingSuccessor`,
  `TestPersistentOwnerRestartAndMemoryOwnerLoss`,
  `TestPersistentAliasCannotBypassOwnerHold`. Test active-stop drain, wait
  cancellation and `hold TARGET -- CMD` through actual built binaries.
- Rerun Windows named-pipe owner smoke on this final candidate and assert the
  corrected `session.stop` destructive envelope. Confirm current-account ACL.
- Run manager's full touched packages, consumer checks and crossvet. The
  adapter constraints in `docs/storage-adapters-evidence.md` apply; category
  stories own data verbs and logical interchange. This story does not claim
  their acceptance.
- Manager's Windows gate on `c6e17898` failed atlas classification (missing
  registered tool/effects/reversibility/stability); this candidate fixes that
  with the focused atlas pass above. The same gate reported `mcp.TestPolicyCalls`
  failures around zero audit duration on Windows; manager reproduced them on
  parent `2111a999`, with unchanged MCP source. That pre-existing failure is
  not marked resolved here. Raw gate:
  `$HOME/sprint453/gate-20261017-213411`.
- Manager's Windows repair gate on `6a43f159` passed lifecycle, command,
  adapter, search and codegraph packages but failed
  `atlas.TestOriginCountsPinned`: `db` was incorrectly counted as Unix.
  This working tree classifies it as Bashy and passes the full native atlas
  package. The manager's remote atlas rerun remains outstanding. Raw gate:
  `gate-20261017-213959`.
