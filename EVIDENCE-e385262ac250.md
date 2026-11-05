# Sprint 562 telemetry emergency repair evidence

Story: #3047 · Story-ID: `e385262ac250`

The frozen emergency source passed the selected cross-platform controls and one
Windows producer canary. This canonical-based integration preserves unrelated
features, but its complete executable/imported-source closure has not been built
or accepted. This note does not claim fleet rollout or full repository acceptance.

## Source and integration identities

- Canonical integration base: `2c22ccb619d432d4f3b71c57e6a65c80c4e3af64`.
- Emergency branch base: `89df473c6035d6b07471a0267e41fdb466d939a0`.
- Accepted frozen Yoke source: `052a0d2363b156fac9484ac017fceffb3354be39`.
- Accepted Bashy runtime source: `83f5a5a176f3c9dd827bd4796d257090298890fc`.
- Bashy test source: `ffd98eb202e22608dff03a9447d79602cf558746`.
- Canary Outpost source: `a0b4a1e6175e1236d619c694c49626d4dde149a2`,
  version `v1.0.0-rc.26-dev`.

Only the 22 changed code/test files under `pkg/telemetry` and `pkg/binmgr` were
imported from the emergency branch. Both package trees and `go.mod`/`go.sum` were
identical between the original bases, so the patch applied without conflict.
The integration omits the unrelated emergency `KB.md` edit. Its affected package
contents and modes match frozen Yoke exactly:

- `pkg/telemetry`: tree `ff250ca2194953ba2c60f736a9898f6b8940a1e1`.
- `pkg/binmgr`: tree `7f98d9857ce26524c71f7abfc76da316822b0797`.

Package equality and unchanged dependency declarations do not prove that every
transitive imported source or complete binary is identical. The canonical base's
unrelated changes remain present. No Go test or build ran on the development host.

## Repair scope

The implementation bounds spool admission and in-place compaction under a
cross-process lock, reports pressure and failures, and adds typed status
observations that preserve health/error transitions while collapsing repeated
polls. Generic command success is not evidence of service health. A producer
acknowledgment connects typed status handling with generic exec-span suppression;
the actual CLI dispatch preserves that context. The readiness-probe path also
uses the typed observation seam.

Windows append handles include the read access needed for in-place compaction.
Lock failures increment dropped-span counters under the existing mutex; the
forced-timeout control releases the lock and checks the resumed gap report.
Concurrency controls accept only the specified deadline-exceeded/drop-marker
combination, retaining other errors as failures.

## Targeted validation and reuse

The final evidence comprises 104 targeted checks across these separate lanes,
all passing with zero skips or failures:

| Lane | Source | Passing controls |
| --- | --- | ---: |
| Native Windows Yoke | Frozen `052a0d23` | 51 |
| Remote Darwin/arm64 portable packages | Frozen `052a0d23` | 47 |
| Native Windows Bashy producer/actual CLI | Runtime `83f5a5a1`, test source `ffd98eb2` | 6 |

These are selected controls, not 104 distinct cross-platform requirements or a
full-package/full-repository gate. The portable lane contains 45 telemetry and
two readiness-probe controls, excluding Windows-only tests and child-helper
entry points. Its test stage took 11.717 seconds; dependency resolution and tests
ran offline. Source inventories and the pinned Go SDK hash were unchanged.

The Bashy test revision changes only
`internal/agentos/supervisord_service_cli_test.go`, correcting the fresh-state
warning expectation. Its production tree excluding Go tests matches runtime
`83f5a5a1`, SHA-256
`ab51df05201b9c436e1633fb21b3b4b33a4d94a33c1237e206b5ff1978bda74a`.
The six controls include the actual runtime binary. The old-binary negative
control produced 12 generic exec records for 12 status polls and failed as
expected; the corrected positive controls passed.

Evidence identities:

- Frozen Yoke archive:
  `35f122396599c61c7523dd21d6f3fd62f3aeaba5c30262e37b29e1e0b7540360`.
- Portable Go 1.27.2 executable:
  `516c1fbb53b5f8f4852f0c3c379d45bcff53a1032e5cd1a9ca1738f03ba07cb1`.
- Portable raw Go test JSON:
  `387e546e8c5452c2a8b2204b16ad7e76446b6fa179761598bf36f08332d99e88`.
- Native Yoke raw Go test JSON:
  `ce73645433226960baa558dab48ecc73651678311fac5cc8a95b4e218d861c0b`.
- Native Bashy raw Go test JSON:
  `8ec3d1e4208d11b79fabad1730872a6e2b37b6b738a56209e89df91c60842c22`.

## One Windows canary

The supported paired lifecycle installed and verified all four executable hashes:

| Artifact | SHA-256 |
| --- | --- |
| Outpost | `87c977b14192b62e273078afb2de15cc37e3d825acf48877b11d7673532e0241` |
| Bashy | `34df0469bc4b4e2af6fb42797bf07d444e414a9290078fa631aca047c95130d8` |
| Bash | `df7b6e0722ee1664db5fdf436f4731465237c3ef4bbaa9ee765a816d38c57978` |
| Sh | `a8417625d6787c903f78d8c751d2579243059b51c5d97dd5fd5bc206081718cb` |

On 2026-11-05, six-sample storage observations around 18:59 UTC on the first
canary and 19:33 UTC on the corrected canary found no rotation scratch files.
The final canary reported healthy after the observation dwell, with a spool of
39,879,422 bytes and 1,320,831,217,664 bytes free at the final sample.

Twelve actual installed-binary status polls produced zero generic status exec
records. Persisted repeat counts increased from 101,468 to 101,965 during the
observation interval; that increase includes concurrent observation activity and
is not attributed solely to the twelve commands. Typed status records remained
visible. A legacy producer was retired through its supported shutdown lifecycle.
The apps, meet, and service monitor were healthy.

This is one Windows canary, not fleet completion. Four optional units remained
failed: `ollama`, `ollama-pool`, `otel`, and `otel-stack`. Their exact pre-rollout
unit-state baseline is unproven; the note does not claim every service healthy
or classify all optional failures as preexisting. Further rollout and the full
canonical-based binary remain separate acceptance obligations.

## Deferred work

Layered retention, protected semantic-memory originals, and calendar rollups are
tracked under follow-up story `88d62fd745c7`. They are not implemented or claimed
complete by this emergency patch. Bash# tests and examples remain the sprint's
primary deliverable.
