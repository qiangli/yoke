# Q8 context isolation: partial delivery

Sprint #379, Story #1524 (`27141ada8535`), 2026-10-08.

The agent KB ring used an empty owner string for both a missing principal
and an unrestricted store. Consequently, `OpenAgentRing(dir, "")` (including
whitespace-only identities) exposed another principal's checkpoint when its
episode was known. Store scope is now explicit: a missing agent-ring owner
returns no pages; the shared repo and host rings retain their existing behavior.

Regression coverage assembles a checkpoint repeatedly for its owner, another
principal, anonymous callers, and other/missing sessions. Separate store tests
exercise both direct Load and List. Broker coverage reloads a persisted sticky
transcript and verifies that only its original principal/session can retrieve,
use, or delete it, and that expiration still applies after restart.

The broker's cache-eviction comment records the residual timing risk: shared
queue contention, model residency and eviction/reload latency can reveal another
scope's activity. Content isolation does not make responses constant-time.
No timing benchmark was added.

## Scope that remains open

At the supplied base `6103baf8716f58e4fcfa97fa080f3a9ad189c87a`, the tracked tree
contains neither `pkg/kbcontext` nor `pkg/sense`; the available history has no
commits for those paths. The existing `pkg/recall.Context` reads its readers
on each call and has no context cache or decision-request prefix cache.
This change therefore does **not** establish Sense observation revocation or
expiry, catalog/context digest invalidation, or KB ring visibility in those
missing cache identities. Integration requires the owning implementation's
commit/path; Q8 as a whole remains open.

The submodule was supplied at detached HEAD. Delivery stays on that HEAD;
there is no push, branch switch, or umbrella pin change.

## Validation

Native development host: darwin/arm64, Go 1.27.1, umbrella workspace mode.

- Before the fix: `go test ./pkg/recall -run '^TestContextPrincipalAndSessionIsolation$' -count=1`
  exited 1; anonymous and whitespace-principal cases each exposed one block.
- After the fix, the same command exited 0.
- `go test ./pkg/kb ./pkg/recall ./pkg/broker -count=1` exited 0.
- `go test ./pkg/cligw -run '^TestCatalogRefreshes' -count=1` exited 0;
  these are the existing catalog-refresh regressions, not a new implementation.
- `go build ./...` exited 0.
- `go vet ./...` exited 0.
- `scripts/crossvet.sh` exited 0: applet coverage, Windows/Linux/macOS vet,
  and AIX compile canaries all passed (cross-compilation on the development host).

No native Windows/Linux suite, full macOS suite, or CI run is claimed.

## Deferred local-model evidence

Sprint 310 remains outside this delivery. It needs the paired bench host and
an `@contain` probe in the bashy image; broker-driven operational task results
for tier S/M/L models (genie smoke, repository fixes, self-management questions),
a per-model pass matrix and operational fields in `models.json`. The benchmark
rows require mini-swe-agent, agent-mini and genie on one tier-M model, context
at least 64k, K=3, one sticky identity, official container harness evaluation,
paired confidence intervals, and one shared evidence record. Tier L awaits the
bench host. These runs must use the designated test hosts, not the development
machine.
