# Sprint 562 Windows paired telemetry rollout

`rollout_producers.ps1` is the generic, manifest-driven wrapper used for the
emergency canaries. It delegates swaps and rollback to Outpost's existing paired
upgrade lifecycle. It does not install prerequisites or stop guessed process IDs.
No older staging or gate scripts are included here.

Wrapper SHA-256:
`b9b08e365b9713136dee0ff1c07d805a69a94d9ff221e71b42f1d7071f09b04e`.

## Private manifest inputs

Keep deployment paths and raw host receipts outside the public repository.
Supply a JSON manifest and its independently recorded SHA-256:

| Field | Required value |
| --- | --- |
| `schema` | `s562-paired-telemetry-rollout/v1` |
| `yoke_commit`, `bashy_commit` | Exact validated source commits |
| `artifacts` | Objects keyed `outpost`, `bashy`, `bash`, `sh`; each has `path` and `sha256` |
| `archive` | `path` and `sha256` for the paired ZIP |
| `validation` | `path` and `sha256` for the native acceptance JSON |
| `install_dir` | Existing directory matching the live daemon's reported executable |
| `deployed` | Keys `outpost.exe`, `bashy.exe`, `bash.exe`, `sh.exe`; each expected installed SHA-256 or literal `absent` |
| `backup_parent` | Private directory for uniquely named verified backups and transaction logs |

Use the four actual, role-specific built executables, with those exact filenames;
do not rename one binary to impersonate another role. Record their module/build
identities and native validation before constructing the manifest. The ZIP must
contain exactly the four filenames at its root, with matching bytes and no extra
or duplicate members. Hashes are lowercase SHA-256 strings.

The native acceptance document uses schema
`s562-telemetry-native-acceptance/v1`, `passed: true`, matching `yoke_commit` and
`bashy_commit`, matching `outpost_sha256` and `bashy_sha256`, and
`artifact_hashes` keyed by all four roles. Its hash binds the exact supplied
receipt; a staged source tree or an invented success field is not validation.
Retain the underlying native test, binary identity, and negative-control receipts.

## Preview and two-phase execution

Preview is the default; it validates inputs and queries the live target without
performing a deployment swap or creating deployment backups:

```powershell
./rollout_producers.ps1 -ManifestPath $manifest -ManifestSha256 $manifestHash
```

After reviewing that exact preview, add `-Execute` with the same inputs. Phase
one creates and verifies backups, invokes `outpost upgrade --local ... --sha256
... --force --no-restart`, and verifies all four installed hashes. Success emits
`paired-swap-complete-restart-required`; it is not a health or rollout verdict.
On a failed transaction with changed installed bytes, the wrapper requests the
supported rollback and checks the prior hashes, preserving failure evidence.

Phase two belongs to an external controller on a fresh connection. Reconcile the
actual installed hashes, daemon version/source identity, running producer and
monitor identities, and pending confirmation state. If restart is still needed,
use the supported lifecycle. Do not infer completion from the original SSH
session's exit status: Outpost may own that transport.

**Automatic watcher caveat:** the existing supervisor checks executable changes
about every 15 seconds and may re-execute even though the explicit upgrade uses
`--no-restart`. That option is not a promise that processes or the connection will
stay unchanged. Inspect actual state before issuing a redundant restart.

Wait for the new daemon's three-minute continuous-uptime confirmation dwell and
verify its reported health on the fresh connection. Then observe bounded spool
and scratch storage, typed status transitions and persisted repeats, and actual
status polls without redundant generic exec spans. Verify required services
separately; daemon health does not assert that every optional unit is healthy.
Preserve receipts for failed or interrupted attempts and each reconciliation.

## Existing evidence

Three native rejection controls passed: missing successful native evidence,
reuse of one executable for Outpost and Bashy roles, and a mismatched ZIP member.
The first and corrected Windows canary pair transactions also succeeded; the
corrected canary completed external reconciliation and the health/storage dwell.
See [the source-bound evidence note](../../EVIDENCE-e385262ac250.md) for exact
source and artifact identities, targeted counts, and the one-canary limitations.
These results do not establish fleet-wide deployment or acceptance of a new
canonical-based complete binary.
