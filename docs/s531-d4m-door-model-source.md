# S531.D4m delivery — live door model sources

Story #2928 / `333c70de59cf`.

An imported door grant is now the model source. `broker.ResolveDoorModel`
resolves `MODEL@DOOR` or `DOOR-door-MODEL` from that grant on each invocation,
using the same mesh/cloudbox routing and WSL Windows-store fallback as
`llm env --from`. Import and repeated share replace the single grant; no
model YAML, environment setup, or secrets.map credential copy is needed.
Unknown/malformed explicit door sources fail rather than falling through to
a local Ollama pull. Bare local model names retain their existing behavior.
The remote server remains responsible for validating served model IDs.

## Cross-repository integration

The genie resolver is in **bashy**, not yoke. Its implementation and regression
tests are committed as `cd1aa1aa` in the isolated checkout
`/tmp/s531-d4m-bashy`. The complete commit is also preserved in
[the integration patch](patches/s531-d4m-bashy-genie.patch), so this yoke
worker's delivery carries all source changes without touching another worker's
checkout. Apply that zero-context patch in bashy
(`git apply --unidiff-zero <path-to-patch>`) and commit with the delivery
trailers in its header when integrating this yoke commit. Alternatively,
fetch and cherry-pick `cd1aa1aa` from the isolated checkout. After publishing yoke, update bashy's yoke module pin using
the normal module-sync workflow; no unpublished pseudo-version is committed.
Both halves are required for the installed genie command to use this feature.

The genie change resolves door sources before legacy YAML, preventing old
copied keys from shadowing grants. Shared endpoints never trigger local door
startup, even when a mesh forward uses the local door's usual port. Context
uses genie's existing 32768-token fallback; no remote model metadata is
invented or cached.

## Focused gate evidence (Dragon, 2026-10-29)

Red:

- Broker regression failed to compile: `undefined: ResolveDoorModel`.
- Genie regression failed: `codex-gpt6-sol@dragon: ok=false err=<nil>`.

Green:

- `go test ./pkg/broker -run 'Test(DoorModel|Share|DoorImport|EnvFrom|WSL)' -count=1`
  passed. Includes HOME-only import, rotation, stale environment, mesh reuse,
  cloudbox, Windows-store WSL resolution, malformed/missing source refusal,
  and the existing share/import/env regressions.
- In the isolated bashy checkout, with a temporary modfile replacing yoke
  with this workspace:
  `go test -modfile=d4m-test.mod ./internal/agentos -run 'Test(GenieSharedDoor|GenieExternal|GenieDoor)' -count=1 -v`
  passed all seven tests, including stale YAML precedence and local-server
  startup avoidance.

Only focused unit tests ran. WSL/Windows behavior uses deterministic seams;
no installed binary, live host, real credentials, or remote service was changed.
The conductor still owns cross-repo integration and installed-host acceptance.
