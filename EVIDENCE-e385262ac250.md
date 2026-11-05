# Sprint 562 telemetry emergency integration candidate

Story: #3047 · Story-ID: `e385262ac250`

This candidate applies the emergency telemetry patch to the canonical source
baseline while preserving its unrelated changes. It is preparation for review,
not a publication, rollout, or acceptance record.

## Source identities

- Canonical integration base: `2c22ccb619d432d4f3b71c57e6a65c80c4e3af64`.
- Emergency branch base: `89df473c6035d6b07471a0267e41fdb466d939a0`.
- Frozen emergency source: `dbe9f5c0e8949bb3d928dc4c677ab9b7f48028a0`.
- Imported patch: only the 20 changed code/test files under `pkg/telemetry` and
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
Native validation is pending in the manager-controlled gate on the frozen
emergency source. No Go tests or builds were run locally for this integration.

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
