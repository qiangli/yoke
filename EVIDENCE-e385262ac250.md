# Sprint 562 telemetry emergency integration candidate

Story: #3047 · Story-ID: `e385262ac250`

This candidate applies the emergency telemetry patch to the canonical source
baseline while preserving its unrelated changes. It is preparation for review,
not a publication, rollout, or acceptance record.

## Source identities

- Canonical integration base: `2c22ccb619d432d4f3b71c57e6a65c80c4e3af64`.
- Emergency branch base: `89df473c6035d6b07471a0267e41fdb466d939a0`.
- Frozen emergency source: `052a0d2363b156fac9484ac017fceffb3354be39`.
- Imported patch: only the 22 changed code/test files under `pkg/telemetry` and
  `pkg/binmgr` between the emergency base and frozen source.

Both affected package trees and `go.mod`/`go.sum` were identical between the two
bases. The patch applies without conflict. The integration retains the canonical
baseline's other changes; it omits the emergency branch's unrelated `KB.md` edit
and replaces its earlier narrative evidence with this note.

## First-repair scope

The implementation bounds spool admission and in-place compaction under a
cross-process lock, reports pressure and failures, and adds a typed status
observation path that preserves health/error transitions while collapsing
repeated observations. Generic command success is not evidence of service health.
The producer acknowledgment connects status handling with generic exec-span
suppression. The readiness-probe path uses the typed observation seam too.

The compatible Bashy producer hookup must ship with this Yoke implementation.
The manager must record the exact accepted consumer and paired executable hashes
before deployment; source availability is not rollout evidence.

## Validation state

Regression controls are authored for rotation, concurrency, interrupted copying,
Windows file-lock/deletion failures, sustained bounded export, typed status
changes, query failures, restart persistence, and the producer acknowledgment.
The manager reports all 56 required Windows-native P0 controls passed on the
exact frozen source `052a0d2363b156fac9484ac017fceffb3354be39` with paired Bashy
`ba00dfff6149a5df26072ff448c7a46b98cf6c0f`. This Windows result does not itself establish acceptance of this canonical-based
variant. The separate portable result is recorded below. No Go tests or builds were run locally for this integration.

Package-tree equality, including file modes, can establish that these two package
sources match the frozen candidate. It does not establish identical binaries or
an identical transitive imported source graph: the canonical baseline contains
other changes. `go.mod`/`go.sum` equality alone is not a closure proof. Final
integration validation and paired producer verification remain required.

## Deferred work

Layered retention, protected semantic-memory originals, and calendar rollups are
tracked separately under follow-up story `88d62fd745c7`. They are not claimed as
implemented by this emergency patch. The first repair must demonstrate bounded
live storage before the sprint resumes its primary Bash# tests and examples.

## Windows append-handle follow-up

The frozen source now includes the Windows append handle read-access correction
and platform-specific open helpers, with accompanying snapshot/platform fixture
updates. The manager reported the native Windows independent-writer control passed
on predecessor `9f1851689915e3786567f0524acaf3df509c16ca`; the final frozen
source subsequently passed the 56-control native gate recorded above. This is a
scoped control result, not full integration or rollout acceptance. No local Go
tests or builds were run while updating this candidate.

## Lock-timeout accounting follow-up

The latest frozen source increments dropped-span counters for lock failures under
the existing mutex. A new authored control forces timeout, releases the lock,
and checks the resumed export's gap report. The concurrency fixture now permits
only a deadline-exceeded result accompanied by the drop marker; other errors
remain failures. The final frozen source passed the native controls recorded above. The manager's
canary uses the frozen source, not this canonical-based integration variant.

## Final native-control fixture update

The final emergency delta changes only `pkg/telemetry/spool_windows_test.go`.
Its diagnostics and fixture behavior are retained exactly in this candidate.
Production package source is unchanged from the preceding frozen revision.
Portable package validation is recorded below. Live paired-producer storage
observation remains a separate obligation. The canonical imported-source closure caveat above still
applies; the Windows result is not attributed to an unbuilt integration binary.

## Portable selected-package result

The remote Darwin/arm64 gate passed all 47 selected portable controls on frozen
`052a0d2363b156fac9484ac017fceffb3354be39`: 45 in `pkg/telemetry` and two readiness
controls in `pkg/binmgr`, with zero skips, missing tests, or failures. The test
stage completed in 11.717 seconds. Windows-only tests and child-helper entry
points were excluded from this portable selection; they were not counted as
portable passes. This was not a full-package or full-repository gate.

The source archive SHA-256 was
`35f122396599c61c7523dd21d6f3fd62f3aeaba5c30262e37b29e1e0b7540360`.
Go 1.27.2 executable SHA-256 was
`516c1fbb53b5f8f4852f0c3c379d45bcff53a1032e5cd1a9ca1738f03ba07cb1`, matching
the existing managed SDK and independently retained SDK bytes. Dependency
resolution and tests ran offline, using private mutable caches and existing
module-cache inputs read-only. The source inventory and SDK hash were unchanged
afterward. Raw Go test JSON SHA-256:
`387e546e8c5452c2a8b2204b16ad7e76446b6fa179761598bf36f08332d99e88`.

These results belong to the frozen source and selected packages. The imported
closure caveat for this canonical-based integration still applies. No test or
build ran on the development host.
