---
name: dag-p0
description: P0 CI/CD roadmap for `bashy dag` itself — orchestrate + spec + gate
default: orchestrate
---

# bashy dag — P0 work, as a self-driving dag pipeline

This file dogfoods `bashy dag` to drive its own P0 roadmap (from
`dhnt/docs/dag-fleet-feedback-and-roadmap.md`). Run it with **no target** and the
default `orchestrate` goal hands the whole batch to an agentic orchestrator, then
gates — everything is taken care of automatically:

```bash
cd /Users/you/projects/poc/dhnt/coreutils
bashy dag dag-p0.md                  # claude orchestrates all of P0, then gates
bashy dag dag-p0.md ORCH=codex       # pick the orchestrator: codex|claude|agy|opencode|aider
```

Each P0 *item* is also its own target (spec + gate), so you can drive them
manually or just inspect status:

- the **description** is the implementation spec (files + acceptance),
- the **body** is the gate — `go vet` + `go test ./pkg/dag/...` must stay green,
- **`Ensure:`** checks the feature actually landed (a source marker), so a target
  stays RED until it's really implemented, then flips GREEN.

```bash
bashy dag dag-p0.md timeout-retries        # one item's spec + gate
bashy dag dag-p0.md --keep-going gate      # full red/green status of all four
```

After the orchestrator finishes, rebuild + reinstall bashy so the new flags are
live in your shell (`cd ../bashy && make build`, then rm→cp→codesign ~/bin/bashy).
The gate itself is source-based (greps + `go test`), so it converges without a
reinstall.

## Tasks

### baseline
Confirm the dag package starts green before any P0 work. No code change.
Effects: read
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
echo "baseline: pkg/dag is green"
```

### orchestrate
The DEFAULT goal — hand the whole P0 batch to an agentic orchestrator, then
gate. Pick the tool with `ORCH=<codex|claude|agy|opencode|aider>` (default
claude); the orchestrator reads this file's per-item SPECs and implements all
four in `pkg/dag` until the gate is green. Re-runnable: items already present are
skipped by the orchestrator, and the body always re-gates.
Requires: baseline
Effects: write, net
Ensure: grep -qi "explain" pkg/dag/command.go && grep -qi "timeout" pkg/dag/parser.go && grep -qi "dry-run" pkg/dag/command.go && grep -Eqi "output-group|::group::" pkg/dag/command.go
```bash
set -e
ORCH="${ORCH:-claude}"
PROMPT="$(cat <<'PROMPT_EOF'
You are implementing the P0 roadmap for `bashy dag`, in the Go module
github.com/qiangli/coreutils, package ./pkg/dag. You are in the coreutils repo root.
Read ./dag-p0.md — it defines four P0 targets, each with a SPEC + acceptance.
Implement ALL FOUR in pkg/dag (edit the .go files AND add tests):
  1) docs-explain   — document the fingerprint cache in doc.go AND add a --explain flag to command.go
  2) timeout-retries — add Timeout (time.Duration) + Retries (int) to Task in parser.go, enforce in engine.go
  3) dry-run        — add --dryrun / -n plan mode to command.go (print the plan, run no bodies)
  4) log-grouping   — add --output-group to command.go/engine.go emitting GitHub ::group::/::endgroup:: markers
Keep everything green after EACH item:
  gofmt -l pkg/dag  (must print nothing); go vet ./pkg/dag/...; go test ./pkg/dag/... -count=1; go build ./...
Do not modify other packages. Work item by item and run the tests between items.
PROMPT_EOF
)"

echo "== orchestrating dag P0 with: $ORCH =="
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
echo "orchestrate: P0 converged with $ORCH"
```

### docs-explain
P0 #1 — document the incremental cache and add `dag --explain`.
SPEC:
- Document the fingerprint cache in `pkg/dag/doc.go` (or a `pkg/dag/README.md`):
  cache key = sha256(abs DAG path) → JSON under `~/.cache/bashy/dag/`; the
  fingerprint folds body + Sources/Inputs content hashes + upstream
  fingerprints; a target skips iff its hash matches AND all Generates exist.
- Add a `--explain` flag to `pkg/dag/command.go`: instead of running, print per
  target in the subgraph whether it WOULD run or is up-to-date and WHY (changed
  source path / missing Generates / no cache entry / body changed). Reuse
  `Cache.Fingerprint` + a stored-vs-current comparison; emit in `--json` too.
ACCEPTANCE: `bashy dag --explain <t>` prints a per-target run/skip reason; doc
names the cache key + location.
Requires: baseline
Effects: read
Ensure: grep -qi "explain" pkg/dag/command.go && grep -Eqi "cache key|\.cache/bashy/dag" pkg/dag/doc.go pkg/dag/command.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### timeout-retries
P0 #2 — per-target `Timeout:` and `Retries:` in the schema, enforced by the engine.
SPEC:
- `pkg/dag/parser.go`: add `Timeout time.Duration` and `Retries int` to `Task`;
  register `timeout`/`retries` in `metaKeys`; parse `Timeout: 90s` (time.ParseDuration)
  and `Retries: 3` (with an optional `backoff=...`).
- `pkg/dag/engine.go`: in `runOne` (or a wrapper) apply `context.WithTimeout`
  when Timeout>0 (a deadline hit → StatusFailed, ExitCode 124, Err "timeout");
  loop up to Retries+1 attempts on failure (optional backoff sleep between).
- Tests: a `Timeout: 1s` body of `sleep 5` fails in ~1s as a timeout; a
  `Retries: 2` flaky body that succeeds on attempt 2 ends StatusDone.
ACCEPTANCE: the two tests above pass; `--list --json` surfaces timeout/retries.
Requires: baseline
Effects: read
Ensure: grep -qi "timeout" pkg/dag/parser.go && grep -Eqi "retr" pkg/dag/parser.go pkg/dag/engine.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### dry-run
P0 #3 — `--dryrun`/`-n` plan mode.
SPEC:
- `pkg/dag/command.go`: add `--dryrun`/`-n`. When set, build the subgraph and
  topo order, compute each target's cache decision (would-run vs up-to-date) and
  effects, and PRINT the plan (ordered targets + decision + the command/first
  body line) WITHOUT running any body. Emit a `plan` result in `--json`.
- Reuse `Graph.Subgraph`/`TopoSort` + `Cache.Fingerprint`/`UpToDate`; the engine
  gains a `DryRun bool` that short-circuits execution and records a plan.
ACCEPTANCE: `bashy dag -n <t>` lists the ordered plan and runs nothing
(verify with a target that would create a file — the file must NOT appear).
Requires: baseline
Effects: read
Ensure: grep -qi "dry-run" pkg/dag/command.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### log-grouping
P0 #4 — CI log grouping for parallel output.
SPEC:
- `pkg/dag/command.go` + `engine.go`: add `--output-group` (and auto-enable when
  `GITHUB_ACTIONS=true`). Capture each target's stdout/stderr (already done in
  parallel mode; do it in serial under this flag too) and flush wrapped in
  `::group::<target>` / `::endgroup::` markers, in topological order, so `-j N`
  output folds cleanly in CI logs instead of interleaving.
ACCEPTANCE: `bashy dag --output-group -j4 <t>` emits one `::group::`/`::endgroup::`
pair per target with that target's output between them.
Requires: baseline
Effects: read
Ensure: grep -Eqi "output-group|::group::" pkg/dag/command.go pkg/dag/engine.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### gate
P0 convergence check — all four items implemented and the whole package green.
Requires: docs-explain, timeout-retries, dry-run, log-grouping
Effects: read
Ensure: grep -qi "explain" pkg/dag/command.go && grep -qi "timeout" pkg/dag/parser.go && grep -qi "dry-run" pkg/dag/command.go && grep -Eqi "output-group|::group::" pkg/dag/command.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
go build ./...
echo "P0 gate: all four items present and pkg/dag green"
```
