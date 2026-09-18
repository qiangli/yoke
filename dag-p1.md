---
name: dag-p1
description: P1 CI/CD roadmap for `bashy dag` — orchestrate + spec + gate
default: orchestrate
---

# bashy dag — P1 work, as a self-driving dag pipeline

Same shape as `dag-p0.md` (now all green): each P1 item is a target whose
description is the spec, body is the `go vet`+`go test` gate, and `Ensure:`
checks the feature landed. Run with no target and the `orchestrate` default hands
the whole batch to an agent:

```bash
cd /Users/you/projects/poc/dhnt/coreutils
bashy dag dag-p1.md                  # claude implements all of P1, then gates
bashy dag dag-p1.md ORCH=codex       # pick: codex|claude|agy|opencode|aider
bashy dag dag-p1.md --keep-going gate # red/green status of all six items
```

P1 is bigger than P0 (6 items); the orchestrator works item by item and is
re-runnable (done items are skipped, the gate always re-verifies). After it's
green, rebuild + reinstall bashy to get the new metadata/flags live.

## Tasks

### baseline
Confirm the dag package starts green before any P1 work.
Effects: read
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
echo "baseline: pkg/dag is green"
```

### orchestrate
The DEFAULT goal — hand the whole P1 batch to an agentic orchestrator
(`ORCH=<codex|claude|agy|opencode|aider>`, default claude), then gate.
Requires: baseline
Effects: write, net
Ensure: grep -qi "matrix" pkg/dag/parser.go && grep -q "\"vars\"" pkg/dag/parser.go && grep -qi "secret" pkg/dag/parser.go && grep -qi "artifact" pkg/dag/parser.go && grep -qi "when" pkg/dag/parser.go && grep -Eqi "file-absent|http-ok" pkg/dag/doc.go
```bash
set -e
ORCH="${ORCH:-claude}"
PROMPT="$(cat <<'PROMPT_EOF'
You are implementing the P1 roadmap for `bashy dag`, in the Go module
github.com/qiangli/coreutils, package ./pkg/dag. You are in the coreutils repo root.
Read ./dag-p1.md — it defines six P1 targets, each with a SPEC + acceptance.
Implement ALL SIX in pkg/dag (edit the .go files AND add tests), reusing the
existing P0/P1.5/P2 machinery (parser metaKeys, Graph, Engine, Cache, contract):
  1) matrix      — Matrix: parameterized targets, expanded into one node per combination
  2) variables   — frontmatter vars: + ${VAR} expansion in metadata (CLI KEY=VALUE wins)
  3) secrets     — Secrets: env injection + redaction of values in captured output/JSON
  4) artifacts   — Artifacts: declared outputs recorded in the result/envelope after success
  5) ensure-docs — document the Ensure: predicate vocabulary + dag-v1 schema-stability note
  6) when        — When: conditional targets (skip, not fail, when the condition is false)
Keep everything green after EACH item:
  gofmt -l pkg/dag (must print nothing); go vet ./pkg/dag/...; go test ./pkg/dag/... -count=1; go build ./...
Do not modify other packages. Work item by item; run the tests between items.
PROMPT_EOF
)"

echo "== orchestrating dag P1 with: $ORCH =="
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
echo "orchestrate: P1 converged with $ORCH"
```

### matrix
P1 #5 — Matrix / parameterized targets (kills the dist/build-all duplication).
SPEC:
- `parser.go`: add `Matrix map[string][]string` to `Task`; register `matrix` in
  metaKeys; parse `Matrix: os=linux,darwin arch=amd64,arm64` (space-separated
  key=csv pairs).
- A pre-`BuildGraph` expansion pass: replace each matrix target with one concrete
  node per combination, named `<target>:<k>=<v>,...` (deterministic order), each
  combination's key=value injected into that node's Env. The original name becomes
  an aggregator target that `Requires` all of its expansions, so `bashy dag build`
  runs every combination. Rewire `Requires`/dependents to the aggregator.
- Surface the expanded nodes in `--list --json`.
ACCEPTANCE: a `build` target with `Matrix: os=linux,darwin` lists `build:os=linux`
and `build:os=darwin`; running `build` runs both with `$os` set in each body.
Requires: baseline
Effects: read
Ensure: grep -qi "matrix" pkg/dag/parser.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### variables
P1 #6 — variables / macros in DAG.md (beyond CLI KEY=VALUE).
SPEC:
- `parser.go`: a frontmatter `vars:` block of `NAME: value` defaults (also accept
  `:=` immediate and `?=` default-if-unset semantics); store on `Document`.
- An expansion pass (after parse, before BuildGraph) that substitutes `${NAME}`
  in metadata values (Requires/Inputs/Sources/Generates/Env) from vars + process
  env + CLI overrides, with CLI `KEY=VALUE` taking precedence over `vars:`.
- Bodies already expand `${VAR}` via the shell, so focus on METADATA expansion.
ACCEPTANCE: frontmatter `vars:` with `BIN: app` makes `Generates: bin/${BIN}`
expand to `bin/app`; `bashy dag t BIN=x` overrides it to `bin/x`.
Requires: baseline
Effects: read
Ensure: grep -q "\"vars\"" pkg/dag/parser.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### secrets
P1 #7 — `Secrets:` injection + redaction.
SPEC:
- `parser.go`: `Secrets:` metadata (list of names); add to metaKeys.
- `engine.go`: before running a target, resolve each named secret (process env
  first; document a `bashy secrets get <name>` hook for the cloudbox vault) and
  inject it into that target's env. REDACT every resolved secret VALUE from the
  target's captured stdout/stderr and the JSON envelope (replace with `***`).
ACCEPTANCE: a target `Secrets: TOKEN` with `TOKEN=s3cr3t` in env gets `$TOKEN`;
if its body echoes the value, the captured output shows `***`, not the secret.
Requires: baseline
Effects: read
Ensure: grep -qi "secret" pkg/dag/parser.go && grep -Eqi "redact|\\*\\*\\*" pkg/dag/engine.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### artifacts
P1 #8 — `Artifacts:` declaration.
SPEC:
- `parser.go`: `Artifacts:` metadata (paths/globs); add to metaKeys.
- `engine.go`/`command.go`: after a target succeeds, resolve its Artifacts
  (relative to Dir) and record them in the TaskResult + the `--json` run item (an
  `artifacts` field). If `$DAG_ARTIFACTS_DIR` is set, copy them there.
ACCEPTANCE: a target `Artifacts: out.txt` that creates out.txt reports
`"artifacts":["out.txt"]` in the `--json` envelope after running.
Requires: baseline
Effects: read
Ensure: grep -qi "artifact" pkg/dag/parser.go && grep -qi "artifact" pkg/dag/command.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### ensure-docs
P1 #9 — document the `Ensure:` vocabulary + `dag-v1` schema stability.
SPEC:
- `doc.go` (and/or a `pkg/dag/README.md`): document the Ensure predicate forms —
  `file-exists`, `file-absent`, `http-ok`, `cmd`, and a bare shell command — with
  one example each, plus a "dag-v1 schema stability" paragraph (additive-only;
  fields may be added; `schema_version` bumps on any breaking change).
ACCEPTANCE: doc.go names all the Ensure predicates and the compat policy.
Requires: baseline
Effects: read
Ensure: grep -Eqi "file-absent" pkg/dag/doc.go && grep -Eqi "http-ok" pkg/dag/doc.go && grep -qi "schema" pkg/dag/doc.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### when
P1 #10 — `When:` conditional targets.
SPEC:
- `parser.go`: `When:` metadata (a shell condition string); add to metaKeys.
- `engine.go`: before running a target, evaluate `When` through the in-process
  shell (exit 0 = run). When the condition is FALSE, mark the target as a NEW
  skip kind (e.g. a `StatusSkipped`-like "condition false") that does NOT fail the
  run (distinct from dependency-failure skip) and does not run dependents-as-failed.
ACCEPTANCE: a target `When: test -n "$DEPLOY"` is skipped (run still ok) when
DEPLOY is unset, and runs when DEPLOY is set.
Requires: baseline
Effects: read
Ensure: grep -qi "when" pkg/dag/parser.go && grep -Eqi "when|condition" pkg/dag/engine.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
```

### gate
P1 convergence — all six items implemented and the package green.
Requires: matrix, variables, secrets, artifacts, ensure-docs, when
Effects: read
Ensure: grep -qi "matrix" pkg/dag/parser.go && grep -q "\"vars\"" pkg/dag/parser.go && grep -qi "secret" pkg/dag/parser.go && grep -qi "artifact" pkg/dag/parser.go && grep -qi "when" pkg/dag/parser.go && grep -Eqi "file-absent" pkg/dag/doc.go
```bash
set -e
test -z "$(gofmt -l pkg/dag)"
go vet ./pkg/dag/...
go test ./pkg/dag/... -count=1
go build ./...
echo "P1 gate: all six items present and pkg/dag green"
```
