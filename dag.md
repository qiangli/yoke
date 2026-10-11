---
name: yoke
description: Build/test/lint targets for yoke as a bashy dag pipeline (agent-first, no Makefile)
type: dag
---

# yoke — DAG task file

yoke has no Makefile — it builds with plain `go` commands. This DAG file is
the agent-first equivalent, runnable with the `bashy dag` task runner:

```bash
bashy dag --list            # available targets
bashy dag build             # build the multicall binary into ./bin
bashy dag test              # test coreutils' own packages (CI scope)
bashy dag crossvet          # cross-OS compile gate (windows/linux/darwin)
bashy dag consumer-build    # consumer-direction gate: build ../bashy vs this tree
bashy dag fmtcheck          # gofmt gate — reports, never rewrites
bashy dag --json test       # machine-readable envelope for an agent
```

**Two gates, not one.** `test` proves the host platform; `crossvet` proves the
ones you are not on. A change is gated by BOTH — `go test` on darwin cannot
see a build-tag break, because the offending file never compiles for windows.
Windows is a shipping target here, so a darwin-only green is not a green.

**A third blind spot: the consumer direction.** Both `test` and `crossvet`
only ever compile coreutils against COREUTILS' OWN module resolution. That is
not the same as being buildable by a consumer. On 2026-08-26 main reached a
state where `cd bashy && make build` FAILED while `test` and `crossvet` stayed
green the whole time: cmds/awk had begun using `interp.Config.DecimalPoint`, a
symbol present only in a newer goawk fork than bashy — a downstream consumer —
pinned. A dependency's own `replace` is ignored by the main module, so bashy
resolved a different goawk and could not compile coreutils' package. Inspection
found the pin itself was rotten: goawk was `replace`d to a two-commit ORPHAN
(`coreutils-locale-numeric`, commit 88712e61a085) with no common ancestor to
the fork's default branch — one branch-deletion from a permanently unbuildable
tree. `consumer-build` closes both holes: it (a) builds the flat sibling
consumer `../bashy` against THIS tree, and (b) refuses a versioned `replace`
whose commit is not an ancestor of the fork's default branch. Run it beside
`test` and `crossvet` before merge.

The default `test`/`vet` scope **excludes the vendored `external/` forks**
(ollama, podman) — they pull cgo + platform-specific backends (MLX, btrfs) and
are upstream's to test; this is exactly the cross-platform CI scope, so the
Windows leg (the product) stays green. `test-all` includes everything for a unix
host with the submodules hydrated.

Resolving the engine: coreutils replaces `mvdan.cc/sh/v3 => ../sh` and the
ollama/podman forks via submodules; inside the dhnt umbrella both are present.

## Tasks

### build
Build the busybox-style multicall binary (`coreutils <tool>` / argv[0] dispatch)
into ./bin. Pure-Go + cross-platform (no external engines), so it builds for
every OS including Windows.
Sources: cmd/, cmds/, tool/, pkg/, git/, shell/, go.mod, go.sum
Generates: bin/yoke
Effects: exec,net,write

```bash
set -e
mkdir -p bin
go build -trimpath -o bin/yoke ./cmd/yoke
```

### test
Test coreutils' own packages — the cross-platform CI scope (excludes the
vendored external/ forks).
Effects: exec,net,write

```bash
set -e
go vet $(go list ./... | grep -v /external/)
go test $(go list ./... | grep -v /external/)
```

### synergy-s8
Run the Sprint 418 S8 shared-context handoff recipe. This is an integration
target: it deliberately uses the existing tool-command envelope, repo KB,
todo, handoff, git, and message board; it does not introduce a second context
log or a special handoff verb.

Set `S8_PLAN_PROMPT` to the request, `S8_KB_SLUG` to a fresh, portable KB
slug, `S8_TODO_UUID` to the todo created for the plan, and `S8_REMOTE_AGENT`
to the paired-host recipient (for example `coder@hostB`). The conductor runs
the two-vendor plan/implement sequence first, preserves the raw JSON records
under `docs/validation/s8/`, commits and pushes the KB/todo carrier, then runs
the paired-host pickup. A remote driver lease is only acquired by the
conductor after `Sprint.CanConverge` allows it.

The codex prompt cites `kb:$S8_KB_SLUG`; the remote message carries that same
slug and the globally resolvable todo UUID. `resume --claim` consumes the
portable handoff file on the paired host.
Effects: exec,net,write

```bash
set -eu
: "${S8_PLAN_PROMPT:?set S8_PLAN_PROMPT}"
: "${S8_KB_SLUG:?set S8_KB_SLUG}"
: "${S8_TODO_UUID:?set S8_TODO_UUID}"
: "${S8_REMOTE_AGENT:?set S8_REMOTE_AGENT}"
mkdir -p docs/validation/s8

# Vendor 1 plans. The exact typed envelope is both the raw evidence and the
# artifact piped into the existing todo/KB/handoff inputs.
bashy tool cmd run claude:plan --json -- "$S8_PLAN_PROMPT" | tee docs/validation/s8/two-vendor-plan.json
bashy kb note add --candidate --type decision --slug "$S8_KB_SLUG" --title "S8 shared plan" --file docs/validation/s8/two-vendor-plan.json
existing_todo_body=$(bashy todo show "$S8_TODO_UUID" --json | jq -r '.body')
printf '%s\n\nPlan artifact: kb:%s\n' "$existing_todo_body" "$S8_KB_SLUG" | bashy todo edit "$S8_TODO_UUID" --note -

# Vendor 2 receives only the portable KB address, not a private Claude session.
bashy delegate codex --json "Implement the plan in kb:$S8_KB_SLUG and record the result against todo:$S8_TODO_UUID" | tee docs/validation/s8/two-vendor-implement.json

# Cross-host pickup: git carries shared KB/todo; MB carries durable pointers;
# handoff carries any uncommitted diff/untracked content as a portable file.
git add docs/kb docs/todo docs/validation/s8
cat > /tmp/s418-s8-recipe-commit-msg <<EOF
docs: preserve S8 shared-context recipe evidence

Sprint: #418
Story: #1870
Story-ID: 5fc8a1fc267e
Agent: ${BASHY_AGENT:?set BASHY_AGENT for the conductor}
EOF
git commit -F /tmp/s418-s8-recipe-commit-msg
git push
cat docs/validation/s8/two-vendor-plan.json | bashy handoff --json -m - --next "Resume kb:$S8_KB_SLUG and todo:$S8_TODO_UUID"
bashy mb send "$S8_REMOTE_AGENT" "S8 pickup: kb:$S8_KB_SLUG todo:$S8_TODO_UUID; pull the carrier, then bashy resume --claim."
```

### crossvet
Cross-OS typecheck of the CI scope WITHOUT needing a Windows/Linux box —
`go vet` compiles every package (tests included) for the target GOOS, which
is exactly the class of break the CI windows leg keeps catching after
darwin-only local work (unix-only types like syscall.Stat_t in untagged
files, or a `//go:build !windows` helper referenced from an untagged test).
`go test` on darwin structurally cannot see that: the file never compiles
for the other platform. Run it alongside `test` before every merge — it is
a gate, not a lint.

The body delegates to `scripts/crossvet.sh`, which is also what the pre-push
hook (`scripts/hooks/pre-push`) execs. One script, two callers: the target
list cannot drift between the manual gate and the automatic one. Read that
script for the target rationale (including the deliberate `aix`
fail-closed-lock canary).

```bash
scripts/crossvet.sh
```

### consumer-build
The consumer-direction gate — the one `test` and `crossvet` structurally
cannot be, because both only ever compile coreutils against coreutils' own
module resolution. A green coreutils does not mean a buildable consumer: MVS
plus the consumer's own `replace`/pin can select a different version of a
shared dependency (a dependency's own `replace` is ignored by the main
module), and the break only surfaces when the CONSUMER compiles coreutils'
packages. That is exactly how `cd bashy && make build` failed on 2026-08-26
over a goawk symbol while every in-repo gate stayed green — see the prose
above. Two checks:

- **(a)** build the flat sibling consumer `../bashy` against THIS working tree
  (its coreutils `replace` is redirected here for the build, then restored),
  failing loudly with the offending package + symbol. Skips cleanly when the
  sibling is absent, so a standalone clone is not blocked. Point it elsewhere
  with `CONSUMER_DIR=/path bashy dag consumer-build` or a positional arg.
- **(b)** refuse a versioned module `replace` pinned to a commit that is not an
  ancestor of the target fork's default branch, naming the branch the commit
  actually lives on — the check that would have caught the goawk orphan before
  it landed. Uses the network only to query the fork's branches; when the fork
  is unreachable it WARNS and skips rather than blocking an offline build.
  Consciously-accepted non-ancestor pins are allowlisted (with a reason and the
  real fix) inside the script; new unlisted ones hard-fail.

The body delegates to `scripts/consumer-build.sh`.

```bash
scripts/consumer-build.sh
```

### fmtcheck
The formatting gate. **Reports; never rewrites** — and that is the whole
design, not caution. gofmt changes doc-comment TEXT as well as whitespace:
godoc's legacy typographic substitution turns a pair of ASCII single-quotes
into a curly closing quote. That has already silently corrupted a comment in
this tree which quoted shell syntax, leaving it documenting a form that does
not exist. An auto-fixing gate lands that class of change unreviewed, wearing
the disguise reviewers skim past.

So the gate fails and a human decides. When a file legitimately cannot take
gofmt's output, restructure it — move the literal into an indented doc-comment
code block, where the characters survive verbatim.

The body delegates to `scripts/fmtcheck.sh`, which is also what the CI ubuntu
leg runs. One script, two callers. Tracked `.go` files only; `external/` is
excluded as vendored upstream.

```bash
scripts/fmtcheck.sh
```

### vet
Static check, same scope as `test`.
Effects: exec,net,write

```bash
go vet $(go list ./... | grep -v /external/)
```

### test-all
Full test including the vendored external/ forks. Needs a unix host with cgo and
the ollama/podman submodules hydrated.
Effects: exec,net,write

```bash
go test ./...
```

### dist
Cross-compile the multicall binary for every release platform into bin/dist/.
Generates: bin/dist
Effects: exec,net,write

```bash
set -e
mkdir -p bin/dist
for plat in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${plat%/*}; arch=${plat#*/}; ext=""
  [ "$os" = windows ] && ext=.exe
  out="bin/dist/coreutils-${os}-${arch}${ext}"
  echo "building $out..."
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o "$out" ./cmd/yoke
done
```

### clean
Remove built binaries.
Effects: destroy

```bash
rm -rf bin
```
