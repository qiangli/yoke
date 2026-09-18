---
name: dag-p2
description: P2 CI/CD roadmap for `bashy dag` — orchestrate (codex) + spec + gate
default: orchestrate
---

# bashy dag — P2 work, as a self-driving dag pipeline

Same shape as the green `dag-p0.md`/`dag-p1.md`. P2 is the leapfrog/distribution
layer; each item is scoped so its CORE is hermetically testable in `pkg/dag`
even though the real backend (S3/podman/outpost-mesh) is external — implement and
TEST the seam, document the backend as the consumer.

```bash
cd /Users/you/projects/poc/dhnt/coreutils
bashy dag dag-p2.md ORCH=codex          # codex implements all of P2, then gates
bashy dag dag-p2.md --keep-going gate   # red/green status of all five items
```

## Tasks

### baseline
Confirm the dag package starts green before any P2 work.
Effects: read
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
echo "baseline: pkg/dag is green"
```

### orchestrate
The DEFAULT goal — hand the whole P2 batch to an agentic orchestrator
(`ORCH=<codex|claude|agy|opencode|aider>`, default claude; run this batch with
`ORCH=codex`), then gate.
Requires: baseline
Effects: write, net
Ensure: grep -Eqi "cache-dir|cache-export|CacheDir" pkg/dag/command.go && grep -qi "exitcodes" pkg/dag/parser.go && grep -qi "watch" pkg/dag/command.go && grep -qi "sandbox" pkg/dag/command.go && grep -q "\"host\"" pkg/dag/parser.go
```bash
set -e
ORCH="${ORCH:-claude}"
PROMPT="$(cat <<'PROMPT_EOF'
You are implementing the P2 roadmap for `bashy dag`, in the Go module
github.com/qiangli/coreutils, package ./pkg/dag. You are in the coreutils repo root.
Read ./dag-p2.md — it defines five P2 targets, each with a SPEC + acceptance.
Implement ALL FIVE in pkg/dag (edit the .go files AND add tests), reusing the
existing machinery (parser metaKeys, Graph, Engine, Cache, contract, results):
  1) cache-portable — relocate the fingerprint cache (--cache-dir / DAG_CACHE_DIR) + --cache-export/--cache-import a local dir
  2) exit-contracts — ExitCodes: metadata mapping a body's exit code to ok|skip|retry|fail
  3) watch          — --watch re-runs the affected subgraph when a Sources file changes (poll); make the affected-set helper unit-testable
  4) sandbox-effects — --sandbox runs bodies through a configurable wrapper (DAG_SANDBOX_CMD), mapping Effects: to constraints; unit-test the arg construction
  5) remote-placement — Host: metadata recorded + surfaced in the --json envelope; a pluggable executor seam defaulting to local in-process bash
For items whose real backend is external (S3/GCS, podman, the outpost mesh),
implement and TEST the local seam (local-dir cache, wrapper-command construction,
Host surfaced) — do NOT require the external system in tests.
Keep everything green after EACH item:
  gofmt -l pkg/dag (must print nothing); go vet ./pkg/dag/...; go test ./pkg/dag/... -count=1; go build ./...
Do not modify other packages. Work item by item; run the tests between items.
PROMPT_EOF
)"

echo "== orchestrating dag P2 with: $ORCH =="
case "$ORCH" in
  claude)   claude -p "$PROMPT" --dangerously-skip-permissions </dev/null ;;
  codex)    codex exec --sandbox workspace-write "$PROMPT" </dev/null ;;
  opencode) opencode run "$PROMPT" </dev/null ;;
  agy)      agy -p "$PROMPT" </dev/null ;;
  aider)    aider --message "$PROMPT" --yes --no-auto-commits </dev/null ;;
  *) echo "unknown ORCH=$ORCH (use codex|claude|agy|opencode|aider)" >&2; exit 2 ;;
esac

echo "== verifying convergence =="
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
go build ./...
echo "orchestrate: P2 converged with $ORCH"
```

### cache-portable
P2 #11 — relocatable + import/export fingerprint cache (CI cross-run persistence).
SPEC:
- `cache.go`/`command.go`: let the cache directory be overridden by `--cache-dir <path>`
  or `DAG_CACHE_DIR` (default `os.UserCacheDir()/bashy/dag`). `LoadCache` takes the dir.
- Add `--cache-export <dir>` (copy this document's cache file into <dir>) and
  `--cache-import <dir>` (copy it back before the run), so CI persists fingerprints
  across runs via an artifact dir. Document S3/GCS as future backends behind the
  same copy seam.
ACCEPTANCE: run a target with Generates once; `--cache-export d`; wipe the cache;
`--cache-import d` then re-run → the target is up-to-date (skipped).
Requires: baseline
Effects: read
Ensure: grep -Eqi "cache-dir|CacheDir" pkg/dag/command.go && grep -Eqi "cache-export|cache-import" pkg/dag/command.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### exit-contracts
P2 #15 — `ExitCodes:` classify a body's exit code into an outcome.
SPEC:
- `parser.go`: `ExitCodes:` metadata, e.g. `ExitCodes: 0=ok 75=skip 2=retry` →
  `map[int]string` on Task (add `exitcodes` to metaKeys).
- `engine.go`: after a body runs, map its exit code: `ok` → done; `skip` →
  condition-skipped (NOT a failure); `retry` → counts as a failure that consumes a
  Retry; unmapped non-zero → failed. Default behavior (no ExitCodes) unchanged.
ACCEPTANCE: a body that `exit 75` with `ExitCodes: 75=skip` ends skipped (run ok);
`exit 2` with `2=retry` and `Retries: 1` re-runs once.
Requires: baseline
Effects: read
Ensure: grep -qi "exitcodes" pkg/dag/parser.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### watch
P2 #12 — `--watch` re-run the affected subgraph on source change.
SPEC:
- `command.go`/`engine.go`: `--watch` runs the requested targets, then polls the
  fingerprints of their `Sources:`/`Inputs:` (interval, e.g. 1s) and re-runs the
  affected targets when anything changes; loops until interrupted (SIGINT).
- Factor the "which targets changed since last run" decision into a pure,
  UNIT-TESTABLE helper (e.g. `changedTargets(prev, cur map[string]string) []string`)
  so the watch loop itself can stay thin and untested.
ACCEPTANCE: the helper returns exactly the targets whose fingerprint changed;
`--watch` appears in `--help`.
Requires: baseline
Effects: read
Ensure: grep -qi "watch" pkg/dag/command.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### sandbox-effects
P2 #14 — `--sandbox` run bodies through a wrapper, mapping `Effects:` to constraints.
SPEC:
- `engine.go`/`command.go`: `--sandbox` (or `DAG_SANDBOX_CMD`) wraps each body's
  execution in a configurable command (the real backend is `bashy podman run`).
  Map declared `Effects:` to wrapper flags by a documented convention: no `net` in
  Effects → network disabled; no `write` → read-only rootfs; etc.
- The wrapper command + its args must be built by a pure, UNIT-TESTABLE function
  (e.g. `sandboxArgs(effects []string) []string`); do NOT require podman in tests.
ACCEPTANCE: `sandboxArgs` adds `--network=none` unless `net` is declared and a
read-only flag unless `write` is declared; `--sandbox` appears in `--help`.
Requires: baseline
Effects: read
Ensure: grep -qi "sandbox" pkg/dag/command.go && grep -Eqi "sandboxArgs|--network|read-only|readonly" pkg/dag/engine.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### remote-placement
P2 #13 — `Host:` placement intent + a pluggable executor seam.
SPEC:
- `parser.go`: `Host:` metadata on a Task (add `host` to metaKeys) — the intended
  placement, mirroring cloudbox `SprintRun.Host`.
- `interp.go`/`engine.go`: define an executor seam so a target body CAN run
  somewhere other than local in-process bash. Keep the default = the existing
  in-process bash interpreter; add a no-op/local "executor" abstraction the future
  outpost-mesh dispatcher plugs into. Surface the resolved `host` in the `--json`
  run item (empty = local).
ACCEPTANCE: a target with `Host: host-a` reports `"host":"host-a"` in `--json`;
with no Host it runs locally exactly as before (all existing tests pass).
Requires: baseline
Effects: read
Ensure: grep -q "\"host\"" pkg/dag/parser.go && grep -qi "host" pkg/dag/command.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### gate
P2 convergence — all five items implemented and the package green.
Requires: cache-portable, exit-contracts, watch, sandbox-effects, remote-placement
Effects: read
Ensure: grep -Eqi "cache-export" pkg/dag/command.go && grep -qi "exitcodes" pkg/dag/parser.go && grep -qi "watch" pkg/dag/command.go && grep -qi "sandbox" pkg/dag/command.go && grep -q "\"host\"" pkg/dag/parser.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
go build ./...
echo "P2 gate: all five items present and pkg/dag green"
```
