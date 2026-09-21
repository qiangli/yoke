# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

**`bashy sprint` is the source of requests, plans and details for every agent** — read the sprint card (spec-ref, acceptance, continuity) for what to do; a todo needs no sprint to exist, sprint work is tracked as stories on the card. Delivery commits carry `Sprint:` / `Story:` / `Story-ID:` trailers.

## Overview

`yoke` is bashy's **agentic userland**: everything the bashy shell adds on
top of the certified POSIX package that is not itself a POSIX-required or
GNU coreutils program. It was split out of
[coreutils](https://github.com/qiangli/coreutils) in Sprint 208 (2026-09-18)
so that yoke can change rapidly with AI advancements while the certified
package (`bash` engine + `coreutils`) stays stable — bug fixes only. The
dependency direction is load-bearing: **yoke imports coreutils
(`tool/`, `multicall/`, `shell/`, `pkg/{weavecli,schedule,lockfile,…}`);
coreutils never imports yoke.** Two package-level variables in coreutils are
the only seams (`weavecli.DetectTool`, set by `pkg/fleet` at init;
`schedule.DefaultAdmission`, set by `pkg/llmbudget` at init) — they are
variables, not interfaces, and there is no third.

What lives here:

- the **AgentOS hub** — `pkg/{fleet,kb,bus,room,meet,weave,dag,foreman,chat,
  skills,craft,secrets,binmgr,atlas,…}` (see §AgentOS hub);
- the **non-POSIX applets and front-door verbs** — `cmds/{ast,browser,fetch,
  jq,tar,gzip,tree,which,watch,tokens,tz,ntp,cal,hexdump,clip,duration,why,
  atq,atrm,graph,foreman,resources}`; `cmds/all` blank-imports coreutils'
  certified `cmds/all` and adds these, so a consumer that wants the whole
  userland imports `github.com/qiangli/yoke/cmds/all`;
- `git/` — the pure-Go git client; `mcp/` — the MCP server (`yoke mcp`);
- `external/` — managed externals, toolchain provisioners, the ollama and
  podman forks (submodules) and the otel stack; `pkg/oci` (nested module);
- `cmd/yoke` — the multicall binary over the WHOLE userland (+ `yoke mcp`).

Membership is ONE rule, recorded row by row in the dhnt umbrella's
`docs/coreutils-required-set.tsv`: a package is here iff it is NOT
POSIX-required ∪ GNU coreutils and NOT in that set's import closure. The
`pkg/atlas` census test spans BOTH modules' `pkg/` so a capability cannot
hide on either side of the split.

This repo is OSS (MIT) and is consumed by other OSS repos (bashy, outpost,
ycode). The same hard rules as coreutils apply — never port GNU source,
never shell out to implement a tool's own behavior, upstream semantics are
immutable — and the same agent contract (deterministic output, GNU exit
codes, unsupported flags fail loudly). Read coreutils' `CLAUDE.md` for the
full statement; this file does not repeat it.

## Build & test

No Makefile — `DAG.md` is the agent-first equivalent (`bashy dag --list` /
`build` / `test` / `crossvet` / `consumer-build`). Plain go:

```bash
go build -o bin/yoke ./cmd/yoke      # the whole-userland multicall (+ yoke mcp)

# CI scope — EXCLUDES the vendored external/ forks (ollama, podman):
# they pull cgo + platform backends and need hydrated submodules.
go vet  $(go list ./... | grep -v /external/)
go test $(go list ./... | grep -v /external/)

scripts/crossvet.sh                  # THE CROSS-OS GATE — alongside go test, not instead
scripts/ci-test-gate.sh              # what CI runs: the no-regression ratchet
```

Flat siblings this module replaces: `../coreutils`, `../sh`, `../filebrowser`
(the `pkg/webconsole` File Browser fork). CI clones all three next door;
inside the dhnt umbrella they are submodules already.

**CI runs the suite through a RATCHET** (`scripts/ci-test-gate.sh` against
`test/known-failures.txt`, per GOOS), exactly as coreutils does — a plain
`go test` gate would be permanently red on tests committed for open
stories. **A change is gated by BOTH `go test` and `scripts/crossvet.sh`.**
Install the pre-push hook once per clone: `git config core.hooksPath
scripts/hooks`. `pkg/dag`'s capacity tests talk to a fixture worker with
short timeouts — run the package alone, not in parallel with another heavy
suite, before reading a failure as real.

**The bashy apps console has BROWSER end-to-end tests, and UI changes need
them** (`go test ./pkg/webconsole -tags verifydom`; Linux CI runs them). The
byte-level tests cannot see the cascade, the DOM, or a script that throws.

**Registered means tested.** Every command package imported by `cmds/all`
carries package-local behavioral tests (`scripts/applet-test-coverage.sh`,
invoked by `crossvet.sh`).

## Architecture

### git/ — the pure-Go git client

Self-contained git client on go-git/v5. Two API layers over one
implementation:

- **Typed functions** (`Clone`, `Pull`, `Merge`, `TagCreate`, `ConfigSet`,
  …) returning `*Result` / typed slices — for CLIs that own their flag
  parsing. outpost's `outpost git` cobra tree consumes these. `CLIName`
  (package var, default `"git"`) is the command prefix used in hint
  messages; embedders override it at init (`outpost git`, `bashy git`).
- **`Exec(ctx, dir, args)`** — argv-style dispatch (`args[0]` =
  subcommand) over `execHandlers` covering ~35 porcelain + plumbing
  subcommands. Returns `ErrUnsupported` for unknown subcommands or
  unparsed flags so callers can fall back to a richer tier (ycode falls
  back to host git, then a container) or report a clear error.
  `ExecCommands()` lists the supported set for callers that register
  per-subcommand dispatch.

Files: `git.go` (typed core: clone/init/add/commit/status/log/push/pull/
fetch/branch/checkout/remotes/show/rev-parse), `merge.go` (ancestry +
`ffUpdate` fast-forward engine + `Merge`), `inspect.go` (merge-base,
rev-list, ls-files, blame, grep, diff, show-patch), `configcmd.go`,
`tag.go`, `worktree_ops.go` (reset, rm), `transport.go`, `exec.go`
(dispatcher + pull/clone argv delegates), `exec_read.go` / `exec_write.go` /
`exec_plumbing.go` (argv handlers).

Non-obvious load-bearing details:

- **`Pull` is hand-rolled, NOT go-git's `Worktree.Pull`** — the upstream
  helper integrates the remote's HEAD instead of the current branch's
  upstream, errors when the local branch is merely ahead, and refuses any
  unstaged change. Ours: fetch → upstream resolution (explicit arg >
  `branch.<name>.merge` > same-name) → ancestry analysis → `ffUpdate`.
- **`ffUpdate` preserves non-conflicting local changes** via a manual
  tree-diff apply with an overlap check, because go-git's `MergeReset`
  refuses all dirt and `HardReset` deletes untracked files. Conflicting
  local edits abort with a git-style "would be overwritten" error before
  anything mutates.
- **`transport.go` replaces the "file" protocol** with go-git's in-process
  server (stock go-git execs `git-upload-pack` for local paths — the exact
  dependency this repo must never have). Includes a dotgit-aware loader
  (serves non-bare repos) and a haves filter (go-git's server errors
  "object not found" when the client advertises local-only commits; real
  git servers ignore unknown haves). `InstalledFileTransportIsPureGo()` +
  `TestFileTransportIsPureGo` pin this — do not remove.
- **Diverged histories are an error by design.** No merge-with-conflicts,
  no rebase in the typed layer (Exec has a linear conflict-free replay
  for cherry-pick/rebase inherited from ycode; it returns ErrUnsupported
  on any conflict rather than leaving a half-applied state).
- **`config` branch.\* keys go through go-git's typed `Branches` map** —
  raw-section writes to typed sections (branch/remote/submodule/url) get
  silently dropped by go-git's `Marshal`. remote/submodule/url keys are
  rejected with a clear error.
- macOS tempdir symlinks (`/var` → `/private/var`) break naive
  `filepath.Rel` against go-git's resolved root — `relToRepoRoot` in
  `exec.go` handles it; keep using it for any new path-taking handler.

### History

`git/` was first built inside outpost (`internal/agent/git`), briefly lived
in the sh fork, and landed here when this repo was created as the shared
home for agent tools. The argv `exec_*.go` layer and its tests came from
ycode's `internal/runtime/toolexec/git_native*.go` (3-tier git tool); the
typed layer and its tests from outpost. ycode's e2e suite
(`git_e2e_test.go`) stayed in ycode and exercises this package through
ycode's executor — run it after changing argv-handler behavior.

## AgentOS hub

yoke is the shared **AgentOS tool hub** over coreutils' registry: one
registry, three consumption surfaces, imported by bashy/ycode/outpost.

- `shell/` — the `interp.ExecHandler` adapter above (in-process; the fast
  lane for Go hosts).
- `multicall/` — busybox argv[0] dispatch (any non-Go agent execs
  `coreutils <tool>` / a symlinked name).
- `mcp/` — an MCP server (`NewServer` / `ServeStdio`, over
  `github.com/modelcontextprotocol/go-sdk`) exposing the registry to non-Go
  agents via generic `list_tools` / `run_tool` meta-tools; `yoke mcp`
  starts it over stdio (it was `coreutils mcp` before the split; the
  certified coreutils binary is multicall-only now).
- `pkg/` — importable pure-Go engines relocated from ycode so one
  implementation serves every host: `pkg/treesitter` (AST symbols/search,
  gotreesitter, no cgo), `pkg/repomap` (token-budgeted file→symbol map),
  `pkg/codegraph` (gfy-backed graph — only its importer pulls gfy's
  document-parsing deps; the bare binary stays free of them),
  `pkg/execlog` + `pkg/spacegraph` (the execution planes — the agentic
  replacement for the interactive-only `history` builtin: every dispatched
  command in order, and the host/endpoint/account entity graph learned from
  it. `execlog` is a STREAM and `spacegraph` a VIEW; **kb is the store of
  record**, and `execlog.PromoteToKB` is the one-way pipe that turns a
  counted observation into a candidate page. The dependency direction is
  load-bearing: a stream may import kb, kb must never import a stream, or
  adding a feeder means editing the store of record. See
  `../docs/knowledge-substrate-reconciliation.md`), and
  `pkg/weave` + `pkg/weavecli` (the filesystem-based multi-agent workspace
  orchestrator — pure-filesystem, depends only on weavecli/cobra/pty, no
  Gitea/loom; `NewWeaveCmd()` is the host-agnostic entry point), and
  `pkg/binmgr` (the shared managed-external-binary mechanism: `Ensure(Tool)`
  downloads → sha256-verifies → caches a per-platform release binary, and
  `Start`/`Launch`/`Process.Stop` supervise it with an optional health probe.
  Stdlib-only. `GitHubSpec` resolves GitHub releases; `URLSpec` resolves
  non-GitHub vendors (e.g. act_runner on dl.gitea.com). Both bashy — the "OS of
  binaries" host — and outpost — the lean mesh supervisor — call it in-process to
  run wrapped tools (loom/Gitea, Zot, SeaweedFS, Kopia, **act_runner** — the
  Gitea CI executor, `external/actrunner`) without compiling those heavy binaries
  into either; it is the download half that complements `external/`'s
  exec-an-already-present-binary wrappers. See
  dhnt/docs/external-binary-builtins.md).

  Newer engines, same pattern (one impl, every host; each pulls its deps
  only into its importers): `pkg/dag` (agent-first task runner — the
  Makefile replacement behind `bashy dag`; this repo's own `DAG.md` +
  `dag-p*.md` are its task files), `pkg/foreman` (process manager over
  dag; its prompts carry a bounded checkpoint + recent window, never the whole
  history, and its state changes are sequenced + digested for `status --wait` —
  see docs/foreman-context-contract.md), `pkg/resources` (live host resource telemetry behind
  `bashy resources system` + the board's `resources` panel — CPU/memory/
  disk/network/GPU read straight from /proc, sysctl, and the Win32 entry
  points, pure Go, sample-and-diff for rates; see docs/resources.md),
  `pkg/schedule` (bashy's modern cron, robfig/cron), `pkg/sdlc`
  (the label-driven SDLC control plane), `pkg/secrets` (the
  cloudbox-vault client behind `bashy secrets`),
  `pkg/ctty` + `pkg/ask` (reach the HUMAN OPERATOR from a process whose
  stdio belongs to an agent harness — `pkg/ctty` is the channel ladder
  (controlling terminal with an `O_NOCTTY` + foreground-pgrp check → GUI
  askpass → nothing), `pkg/ask` is the `bashy ask` verb: the request, the
  out-of-band rendezvous, and the 0600 sinks. Local-first: no network, no
  pairing — it is NOT a local vault and must not become one. Two rules run
  through both: a GUI counts only when it is ATTENDED by the caller (a
  dialog on a remote machine's screen is worse than none), and a prompt
  result needs POSITIVE evidence (osascript exits 0 when nobody answers).
  See dhnt/docs/bashy-ask-human-input-design.md), `pkg/skills` (the dhnt
  skill-CNL mechanism), `pkg/kb` (the host-scope shared knowledge base
  behind `bashy kb`: OKF-style wiki pages under `~/.bashy/kb` — the
  collective memory of all agents on the host across all repos;
  reconcile-on-write, supersede-not-delete, candidate→validated ladder;
  weave/foreman auto-inject its top matches at spawn — see
  dhnt/docs/kb-host-knowledge-base-design.md. The `sources` + `transfer`
  verbs structure agent-to-agent knowledge transfer: `sources` probes the
  private-memory stores on the host (Claude memory / ycode memex / weave
  memory / repo graph — best-effort, read-only), `transfer` prints the
  retro-shaped checklist (sources + `xfer:<source>` tag counts + related
  pages + literal commands); both write NOTHING — the hard rule "kb reads
  foreign stores, never writes them" is pinned by tests, and the judgment
  half lives in bashy's embedded `knowledge-transfer` skill — see
  dhnt/docs/kb-knowledge-transfer-design.md),
  `pkg/bus` (the agent notification bus behind `bashy bus`: `publish` a change,
  `watch` for one. The PUSH half of how agents coordinate, where `pkg/kb` is the
  durable PULL half — kb holds what is TRUE, the bus carries what just CHANGED.
  Transport is the pkg/room append-only timeline, so there is no daemon to be
  down and a watcher that was not running still drains what it missed; drain
  cursors are per-subscriber, which is what makes it a bus and not a queue.
  Replaces the never-reachable `pkg/notify`, whose intended subscriber name
  `watch` collided with the classic watch(1) — hence the `bus` parent. The
  SIDECAR (`bus subscribe`/`sidecar`/`pending`) is the attention half: an agent
  mid-turn cannot decide to go check a channel, so the sidecar holds its
  subscription, matches topics, applies governance + rate rules OFF its critical
  path, and leaves it a pre-resolved buffer to read at a turn boundary.
  Interrupt-tier delivery is ESC + steer over the coach's control socket. The
  rule everything hangs off: DEMOTE, NEVER DROP — an unauthorized, rate-limited,
  undeliverable or failed interrupt becomes a queued notification with a recorded
  reason, because a dropped message leaves an agent on stale assumptions. The
  turn-boundary hook is `bus.Prepend` in `chat.Session.Say` — the one control
  surface every steer lands on, so meet/weave/foreman/operator all get it once;
  sessions are matched by CONTROL SOCKET, never by name, since names are reused
  across runs and a stale subscription would cross-deliver),
  `pkg/chat` + `pkg/ollm` (agent chat / Ollama
  client isolation), `pkg/browser` + `pkg/webinspect` (browser automation
  for `cmds/browser`/`fetch`, three modes: `probe` = attach to a Chrome on
  `--remote-debugging-port`, `solo` = launch a headless Chrome, and `live`
  = the MV3 Chrome extension + WebSocket hub that drives the user's real
  logged-in Chrome — `pkg/browser/live`, migrated verbatim from ycode
  Apache-2.0, run via `bashy browser hub` + `--mode live`; the whole browser
  feature now lives here, not ycode), `pkg/coopauth` (the ONE
  shared cloudbox/outpost cooperative-auth impl), `pkg/bre` (POSIX BRE →
  Go regexp, shared by grep/sed), `pkg/ignore` (opt-in agentic path
  filter shared by grep/find), `pkg/mirror` (see below), `pkg/timezones`,
  `pkg/jobs`, `pkg/agentcmd`, `pkg/oci` (separate module wrapped by the
  podman engine).
- `cmds/ast` — the `ast` code-intelligence command with subcommands
  (`ast symbols` list / `ast search` / `ast refs` / `ast map` repo map /
  `ast query` tree-sitter S-expr) over the `pkg/{treesitter,repomap}` engines,
  reachable through all three surfaces (the former flat list-symbols/…/ast-query
  verbs were collapsed 2026-07, mirroring `graph`).
- `cmds/graph` — the `graph` command with subcommands (one registered tool
  that sub-dispatches `graph <sub>`; the former flat `graph-*` verbs were
  collapsed 2026-07). Two layers:
  - **Read (code-graph):** graph build / graph stats / graph neighbors /
    graph impact / graph path / graph hotspots / graph query over `pkg/codegraph`.
    Fully structural + model-free; a bashy-owned disk cache
    (`.agents/bashy/graph.json`) with mtime staleness makes repeated calls cheap.
    `graph_sha` in the `bashy-graph-v1` envelope is a **source** fingerprint
    (relpath|size|mtime), NOT a graph-content hash — gfy assigns node-ids/edge-order
    non-deterministically per build, so only a source fingerprint is reproducible
    across rebuilds (the caching premise is that the graph is a pure function of
    source).
  - **Write (contribution layer, `contrib*.go`):** graph note / graph link /
    graph observe / graph forget (write) + graph recall / graph notes /
    graph pitfalls (read) — the "agentic wiki, built by agents, for agents." A
    durable, append-only JSONL store at the repo root
    (`.agents/bashy/graph/contrib.jsonl`, resolved by walking up to `.git` so all
    agents in the repo share one store). O_APPEND = concurrency-safe multi-agent
    writes without a lock; reads replay the log applying forgets (soft-delete) +
    last-writer-wins per deterministic content id. Deliberately SEPARATE from the
    code-graph cache so contributions survive a rebuild (the clobber hazard).
    Provenance (`by`/`at`/`source`/`confidence`/`episode`) on every record. Pure
    stdlib — no new deps, no gfy pull (bare binary stays clean). `bashy-graph-contrib-v1`
    envelope. See `dhnt/docs/repo-knowledge-graph-design.md` (P1) +
    `dhnt/docs/execution-knowledge-graph-design.md`.
  - **Placement invariant:** `cmds/graph` is **NOT in `cmds/all`** (the read layer
    pulls gfy's document-parsing deps, which must stay out of the bare
    `cmd/coreutils` multicall binary — verified: `go list -deps ./cmd/coreutils`
    has zero gfy pkgs). Blank-imported only by bashy's `internal/agentos`, so the
    `graph` verb reaches the `bashy graph …` front door + in-shell ExecHandler while
    the bare binary and the `bash` drop-in stay gfy-free. See
    `dhnt/docs/bashy-code-graph-agentic-feature.md`.

### external/ — managed externals + embedded forks

`external/` holds three kinds of packages. First, **binmgr-managed
externals** — thin wrappers that `pkg/binmgr`-provision (download →
sha256-verify → cache → exec/supervise) a pinned release, so bashy/outpost
run them without linking them: services (`loom`/Gitea, `actrunner`,
`zot`, `seaweedfs`, `kopia`, `rclone`, `registry`-catalogued k8s/cloud
CLIs), CLIs (`gh`, `helm`, `kubectl`, `act`, `mise`, `curlbin`,
`gitscm` — real git / MinGit on Windows, `meshagent` — execs the outpost
mesh agent without linking it), and **toolchain provisioners**
(`gotoolchain`, `node`, `python` via uv, `rust` via rustup, `java`/Temurin
+ Maven, `clang`, `cmake`) so `bashy <tool>` works on a bare node.
Second, dhnt **front-doors**: `sphere` (the sphere tier) and `tessaro`
(account pairing). Third — distinct from download-and-run — **ollama and
podman are embedded forks we own**, for version/issue control and
identical cross-platform behavior:

- **`external/ollama`** — in-process ollama server + embedded runner; consumes
  the `qiangli/ollama` fork (`external/ollama/src` submodule, `replace
  github.com/ollama/ollama => ./external/ollama/src`). `NewManagedOllamaCmd()` is
  the **isolated** front-door bashy mounts (own bashy-owned port, never 11434;
  models under `~/.agents/bashy/ollama`).
- **`external/podman/engine`** — the in-process libpod/buildah engine + machine
  lifecycle (relocated from ycode's `internal/container`), consuming the
  `qiangli/podman` fork (`external/podman/src` submodule, `replace
  go.podman.io/podman/v6 => ./external/podman/src`) and the `pkg/oci` wrapper
  module (`replace github.com/qiangli/yoke/pkg/oci => ./pkg/oci`).
  `engine.NewPodmanCmd()` is the front-door: pass-through to the embedded podman
  with `CONTAINER_HOST` pinned to an **isolated `bashy` machine** (never the
  host/ycode engine). `$BASHY_PODMAN_SYSTEM=1` defers to a host podman. The helper
  binaries (podman/vfkit/gvproxy) build via `scripts/embed-{podman,vfkit,
  gvproxy}.sh` into gitignored `*_embed/*.gz` blobs, consumed only under the
  `embed_*` build tags (default build uses the stub → host/PATH fallback).
  **The forks pull the go floor up (currently 1.26.4)** — all consumers
  must match.
- **`external/podman/winhelper`** — the Windows helper provisioner
  (gvproxy + win-sshproxy from containers/gvisor-tap-vsock, binmgr-fetched
  and digest-pinned), deliberately kept OUT of `engine` so it imports only
  `pkg/binmgr` + stdlib. It has to be reachable from the build that does
  NOT link the engine: bashy's shipped Windows binary does not link libpod,
  so `bashy podman` *execs* a podman instead, and that
  exec path needs the same provisioning. `Apply(cacheDir)` stages the
  helpers and exports `$CONTAINERS_HELPER_BINARY_DIR` (+ PATH).
  **Why it is load-bearing:** on Windows podman does not serve its API
  itself — `podman machine start` launches `win-sshproxy.exe` to forward
  the VM socket onto `\\.\pipe\podman-<machine>`,
  `\\.\pipe\docker_engine`, and `%TEMP%\podman\<machine>-api.sock`. It
  finds that helper only via `helper_binaries_dir` /
  `$CONTAINERS_HELPER_BINARY_DIR`, and with neither the start still
  **reports success while publishing no endpoint at all**. `engine`'s
  `ensurePlatformHelperBinaries` delegates here so the pinned version +
  digests can't drift between the linked and exec'd paths.

The forks are why the canonical test scope excludes `external/` (see
Build & test): they pull cgo + platform backends (MLX, btrfs) and are
upstream's to test; the CI/Windows scope must stay green without them.

Hosts: `bashy` (the AgentOS shell binary) wires `shell.Handler()` so the
whole userland + code-intel verbs run in-process, and mounts `pkg/weave` as `bashy weave`
(`ycode weave` is now a deprecation stub pointing here). ycode's loom MCP
substrate (`pkg/loom`, gitea backend — separate from `pkg/weave`) routes its
client-side git through `coreutils/git` (pure-Go-first, host-git fallback).

## pkg/mirror + external/rclone — the directory mirror

`pkg/mirror` is a continuous one-way directory mirror (node B keeps a live replica
of a dir on node A). It reuses Syncthing's *architecture* — a recursive fs watcher
+ a periodic full-scan backstop + delta transfer — from **all-permissive parts and
no Syncthing code**: `rjeczalik/notify` (MIT) for the recursive watch, a
binmgr-managed `rclone` (MIT, `external/rclone`) for `rclone sync` (delta + mirror
semantics), and our own debounce/backstop/lifecycle orchestration. `bashy mirror
--source <dir> --dest <rclone-target>`; over the mesh, the replica runs `bashy
rclone serve webdav <dir>` exposed as a mesh service and the source points `--dest`
at it. `external/rclone` is also a transparent passthrough (`bashy rclone …`).
