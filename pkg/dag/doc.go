// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package dag is an agent-first task runner: a Makefile replacement whose
// targets are defined as headings in a markdown file (DAG.md) and executed as
// a real dependency DAG. It is built for AI agents first, humans second — the
// inverse of make/task/just.
//
// Why it exists:
//   - Targets are plain markdown an agent already reads and writes fluently
//     (the xc "docs ARE the tasks" idea), so adding a target is appending a
//     heading + a fenced code block. No tabs, no proprietary DSL.
//   - Discovery is structured: `dag --list --json` returns the full target
//     inventory (names, descriptions, requires, sources/generates) so an agent
//     plans against the real graph instead of scraping `make help`.
//   - Results are structured: every run emits a weavecli.Envelope with stable
//     exit codes and per-target status, forced to JSON by BASHY_AGENTIC=1.
//   - Bodies run hermetically through the in-process mvdan.cc/sh/v3 fork with
//     the coreutils userland resolved in-process (shell.Handler()) — no PATH
//     variance, identical on Linux/macOS/Windows.
//
// P1 (this code) is the minimal-but-real DAG engine: parse -> build graph ->
// cycle detection -> topological SERIAL execution -> bash-in-process -> envelope
// output. Later phases layer parallel scheduling + fingerprint skip (P1.5), the
// dhnt contract/effects/attestation model (P2 — each target may declare an
// `Ensure:` postcondition and an `Effects:` cap), and multi-interpreter bodies
// via RegisterInterpreter: a ```bashpp body runs as Bash++ and may declare
// `~~~py`/`~~~typescript` fences whose functions it calls directly, which is
// how a task reaches another language without a second body interpreter.
//
// # Working directory
//
// Bodies run in the INVOKING working directory, as make recipes do: `-f FILE`
// or a positional file only selects the task file, so a task file kept
// elsewhere (a checked-in example, a shared pipeline) drives the checkout you
// are standing in. To run a graph against another directory use bashy's
// `awd DIR -- bashy dag …` — there is deliberately no `-C DIR` flag. The one
// exception is a positional DIRECTORY (`bashy dag .bashy/deploy target`),
// which names both the file and the place to run it. Fingerprint paths
// (`Sources:`/`Generates:`/`Inputs:`/`Artifacts:`) and `Ensure:` checks resolve
// against that same directory; `include:` and `chunks.json` stay file-relative
// because they describe the file, not the run.
//
// # Includes: sharing one build graph across repositories
//
// A file's frontmatter may pull in targets defined elsewhere, so several
// projects share one graph instead of each copying the same build/test/release
// targets. Local definitions win name collisions, so a repo overrides only what
// differs from the shared set.
//
//	---
//	include:
//	  - gh:qiangli/bashy@v0.19.0/ci.dag.md   # shared, pinned
//	  - ./local-overrides.dag.md             # relative to this file
//	---
//
// The inline form (`include: a.md b.md`) is equivalent for short lists.
// Include resolution is cycle-safe and dedupes diamonds: a file reached twice
// is merged once.
//
// Remote includes must be pinned with `@ref` (tag, branch, or commit SHA);
// bare URLs are rejected. An included target's body is executed, so an
// unpinned reference would let a shared graph change the behavior of every
// dependent repo with no commit in that repo. Resolution is offline-first —
// a pinned ref is immutable by convention, so a cached copy is reused without
// a network call, which is what lets a CI runner or QA host parse the graph
// with no network. Cached under DAG_CACHE_DIR (else the user cache dir),
// keyed by owner/repo/ref/path.
//
// # Incremental fingerprint cache
//
// dag's up-to-date skip is content-hashed, not mtime-based (make's prerequisite
// model): a touched-but-unchanged source does not force a rebuild. The store is
// one JSON file per DAG document (see [Cache] / [LoadCache]):
//
//   - Cache key (the JSON file name): sha256(absolute DAG-file path) hex-encoded.
//     One document → one cache file, so two checkouts of the same pipeline at
//     different paths never collide.
//   - Location: os.UserCacheDir()/bashy/dag/<key>.json — on Linux that is
//     ~/.cache/bashy/dag/, on macOS ~/Library/Caches/bashy/dag/, on Windows
//     %LocalAppData%\bashy\dag\. Writes are atomic (tmp + rename).
//
// Inside the file, Hashes maps a target name to its last successful
// fingerprint. A target's fingerprint (see [Cache.Fingerprint]) folds, in
// order: its body, then each dependency's already-computed fingerprint (so an
// upstream change invalidates everything downstream), then the content hash of
// every Sources/Inputs path (a file's bytes, or a directory's recursive file
// contents). A target is up-to-date — and is skipped — iff it declares
// Generates, all of those outputs exist on disk, AND its recorded fingerprint
// matches the freshly computed one. A target with no Generates is phony and
// always runs. `--force` (-B) ignores the cache entirely; `--explain` prints,
// per target, whether it would run or is up-to-date and why, running nothing.
//
// # Contracts: Require: / Ensure: / Effects:
//
// A target's contract is design by contract at the process boundary: a
// `Require:` line is a precondition evaluated BEFORE the body (a failing one
// means the body never runs), an `Ensure:` line is a postcondition the engine
// evaluates AFTER the body exits 0 (P2 contract): a clean exit is necessary but
// not sufficient — if any Ensure check fails the target fails, even though make
// would have called it done. Both clauses fail the target with the precondition
// exit code (3) and a message naming the clause and the check
// (`precondition failed: <check>` / `postcondition failed: <check>`).
// `Effects:` is the declared effect cap, recorded on the attestation. A target
// may carry more than one `Require:`/`Ensure:` line; all must pass, and order
// carries no meaning. `Require:` (singular) is not `Requires:` — the latter is
// make's prerequisite list, a precondition the engine satisfies by running
// another target. The recognized predicate forms, shared by both clauses, are:
//
//   - file-exists <path>   — the path exists (relative to the DAG-file dir).
//     Example: `Ensure: file-exists dist/app` (also `file-exists path=dist/app`).
//   - file-absent <path>   — the path does NOT exist (e.g. a clean target removed
//     it). Example: `Ensure: file-absent dist/stale.tmp`.
//   - http-ok <url>        — an HTTP GET returns 2xx (a readiness probe).
//     Example: `Ensure: http-ok http://localhost:8080/healthz`.
//   - cmd <shell...>       — an explicit shell command; exit 0 = pass.
//     Example: `Ensure: cmd test "$(cat VERSION)" = 1.2.0`.
//   - <bare shell command> — anything not matching the forms above is run as a
//     shell command through the in-process userland; exit 0 = pass.
//     Example: `Ensure: test -s dist/app && ./dist/app --version`.
//     Example: `Require: test -n "$VERSION"`.
//
// The `file-exists`/`file-absent`/`http-ok` sugar also accepts the explicit
// `key=value` spelling (`path=`, `url=`). See contract.go for the evaluator.
//
// # Fleet execution
//
// `--fleet` runs targets through a [Pool] of [Worker]s instead of a bare -j
// semaphore. A worker offers one or more execution venues; a [Transport] is how
// a target reaches one. `Venue:` / `Lane:` accepts userland, workspace,
// sandbox, native, cluster, and cloud as placement intent. Only the userland
// transport ships today — same host, in process — so `--fleet` on one box is
// `-j N` by construction: `Pool == nil` degrades to LocalPool(Concurrency),
// and there is no second code path to drift.
//
// `Distribution:` preserves the dhnt.pipeline/v1 expansion intent: single,
// shardable, replicated, or topology-coupled. Legacy DAG execution permits it
// to be absent; a DKS/pipeline lowering opts into [TaskSpec.ValidateForPipeline]
// and fails closed when it is missing or unknown. The DAG package records and
// explains this contract but does not invent a lowering or executor for it.
//
// The pool is the gate, not an addition to one. It owns placement and per-worker
// slot accounting, which a single global semaphore cannot express ("4 slots
// here, 12 there"). [Pool.Eligible] fails a target fast when no worker could
// ever host it, rather than parking it on a slot that will never qualify.
//
// Chunking is a separate axis and pays off with no fleet at all. Chunk
// membership — which cases are in chunk i, out of how many — is a property of
// the CORPUS, pinned in a committed manifest (see [LoadChunkManifest]) and
// changed only when the corpus changes. Fleet capacity decides how many chunks
// run concurrently, never how many exist or what is in them. Deriving membership
// from capacity would make `suite:shard=7` name a different case set depending
// on who was online, which breaks both selective re-run and the fingerprint
// cache. [BindChunks] hands each shard target its committed case list via
// DAG_CHUNK_MEMBERS; a manifest that reaches no target is an error, because
// silently dropping corpus produces a flattering pass rate.
//
// # Run journal
//
// Without a journal dag is write-only to the terminal: [Cache] keeps
// fingerprints and durations, [RunReport] is returned and dropped, and per-step
// output has no file at all — so when a run ends there is nothing left to look
// at. [Journal] records each run under the SAME root the fingerprint cache uses
// (see [ResolveCacheDir]), one directory per run: report.json ([RunEntry]),
// graph.json ([RunGraph], with each target's topological layer precomputed so a
// renderer needs no graph algorithm), and logs/<target>.<attempt>.log.
//
// Read it back three ways, none of which runs anything:
//
//   - `--runs`   — the list, newest first ([ListRuns]).
//   - `--status` — the glance: ONE line for the most recent run, exiting
//     non-zero when it failed, so it composes in a shell. A status verb that
//     reported failure with exit 0 would be the same absence-of-evidence shape
//     this package refuses in RecordAttempt.
//   - `--show <run-id>|last` — the detail ([LoadRun]); add `--html` for a
//     standalone page with the graph laid out by layer.
//
// [ListAllRuns] is the machine-global view across every dag document on the
// host, which is what a steward board consumes; [ReadRunLog] reads one
// attempt's output back.
//
// The HTML view is deliberately a layered TABLE, not a drawn graph: each node
// already carries its topological layer, so there is no layout algorithm and
// nothing to get wrong. It embeds its own stylesheet and loads nothing at view
// time — a viewer that needs the network is a viewer that does not work on the
// machine that ran the job.
//
// # Live monitoring
//
// `--serve[=ADDR]` starts a read-only viewer over the journal (loopback by
// default — that is the security model, not a default to relax). It is a
// VIEWER, not a runner: runs are started from a terminal or CI exactly as
// before, and the server shows whatever the journal records from any shell on
// the machine. A server that could start work would be a second execution
// engine with its own scheduling and its own divergence from the CLI.
//
// What makes a run watchable while it happens is [Engine.Observer]: report.json
// only exists once a run is over, so the engine also appends [Event]s to
// events.jsonl as it goes, and the graph is written BEFORE anything runs. A run
// directory holding events but no report is therefore in flight (or was
// killed) — that is how the viewer knows. The server tails the log and pushes
// it over SSE; the page is fully rendered server-side first and the stream
// resumes from that position, so nothing is shown twice or missed.
//
// Observer is nil by default and the engine is inert without it. Set it to
// [Journal.Observer] to record; note it is called from the scheduler's
// goroutines, so an implementation must be concurrency-safe and must not block.
//
// EVERY target reports, including ones that never run a body: a cache hit, a
// dependency-skip, and a false condition all emit their terminal event (see
// noteResult). Omitting them would leave a live viewer showing "pending" for
// most of an incremental re-run, which is the common case.
//
// Output is watchable too, which is the point for a long stage: task.start
// carries the attempt's log path, and the server tails that file over SSE. Per-
// target status changes a handful of times in three hours; the output is what
// an operator actually watches.
//
// Three properties are load-bearing:
//
//   - It is best-effort. A journal that cannot be written never fails a run that
//     otherwise succeeded, exactly as a missing cache dir only means "everything
//     is out of date". Engine.Journal == nil disables it entirely.
//   - It inherits [RunRecord]'s discipline. report.json carries no hostname and
//     no raw error text — a record stamped with the machine that produced it
//     cannot be compared against one produced elsewhere, and free error text is
//     where hostnames and paths leak. Classification lives in the records; prose
//     lives in the log file, which is local and never travels.
//   - Retention is not optional. dag runs on hosts nobody is watching, so
//     [Journal.Finish] prunes to the newest [DefaultKeepRuns] (--keep-runs).
//
// Targets declaring Secrets are journaled differently on purpose: their output
// is redacted only after capture, so it is never teed live (see [journalMayTee])
// and is written once, redacted, when the target finishes.
//
// # dag-v1 schema stability
//
// Every dag envelope stamps schema_version = "dag-v1" (see [SchemaVersion]).
// The compatibility policy is additive-only: new fields may be added to the
// envelope, the list/run/plan results, the attestation, or a target's metadata
// without bumping the version — agents MUST ignore unknown fields rather than
// reject them. schema_version bumps only on a breaking change: removing or
// renaming a field, changing a field's type, or altering the meaning of an
// existing value. New target metadata keys (Matrix/Secrets/Artifacts/When and
// future additions) and new Ensure predicate forms are additive and do not bump
// the version; a reader that does not understand one simply does not act on it.
package dag

// SchemaVersion is stamped into every dag envelope's schema_version field.
// Independent from weave's "loom-v2" — bump when the dag output shape changes.
const SchemaVersion = "dag-v1"
