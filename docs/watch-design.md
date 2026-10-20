# bashy watch — one generic watch engine

Status: Phase 1 implemented (`pkg/watch`, `pkg/watch/hostsrc`, `cmds/watch`).
Phase 2 is scoped at the end and deliberately not built.

## The problem

A conductor supervising several sprints wrote a poll script: every 60s it
grepped a card, an inbox and a process manager, diffed the text against a
baseline and exited on the first new line. In one night it failed four ways:

- a filter that never matched, so a real change was invisible;
- a **missing source** — messages addressed to the operator were not polled at
  all, and a sprint sat idle for most of a night;
- an **empty snapshot** (the command failed) diffed against a full baseline, so
  the whole history replayed as "new";
- **binary bytes** in one command's output broke `grep`.

Every agent reinvents that loop (until-loops, poll scripts, pgrep waiters), and
all of them share the last and worst defect: they cannot see **absence**. A
stalled session emits nothing, so a watcher built on "tell me when a line
appears" reports healthy forever.

## The shape of the answer

One engine, no per-command watch verbs. `watch` already exists as the
GNU-procps applet; it gains an agentic mode. Anything that can *show* its state
can be watched, and anything that already *journals* its state changes can be
subscribed to. A command becomes watchable by emitting to the journal it
already owns — it never implements watching.

```
sources ──▶ events ──▶ rules ──▶ alerts ──▶ sinks
 poll        common      match      fire      console
 mb board    model       change     clear     ndjson
 sprint                  inverse              inbox
 thread                  correlation          until (exit)
```

Four small interfaces, one data model between them. The engine knows nothing
about sprints, messages or processes; the sources know nothing about rules.

## The event model

Every event, whatever its source, carries the same fields (`pkg/watch`,
`Event`):

| field | meaning |
| --- | --- |
| `at` | when it was observed |
| `source` | `poll`, `mb`, `sprint-thread`, … |
| `locator` | the resource, in lens locator spelling: `sprint:480`, `story:ID`, `seat:NAME`, `run:repo-N`, `@host`, a path |
| `kind` | `added` \| `removed` \| `changed` \| `sample` \| `absent` |
| `key` | the record's identity within its source |
| `field` | which field changed (empty for whole-record events) |
| `before` / `after` | the values, field-level |
| `text` | a short human line |
| `attrs` | source-specific labels (`from`, `to`, `topic`, `conv`, `seq`, …) |
| `raw` | the record as it was read |

`attrs` is what keeps the engine generic. A rule predicate matches on labels it
is handed; it never knows that `to` came from a message board. Adding a source
adds labels, not engine code.

### Why `sample` and `absent` are kinds

`sample` is "I observed a value, unchanged" — a threshold or rate rule needs
those, a change rule ignores them. `absent` is an observation a source can make
on its own (a poll whose target vanished). Neither is an alert: alerts are what
rules produce.

## Sources (Phase 1)

A source is one method:

```go
type Source interface {
    Name() string
    Poll(ctx context.Context, now time.Time) ([]Event, error)
}
```

Poll, not callback, for three reasons that all come from the same place: the
backing stores are append-only local files. A reader that was not running when
a record landed still sees it on the next read; there is no daemon to be down;
and a fake clock plus a fixture directory is a complete test harness. Phase 2's
fsnotify source is push, and gains a streaming variant of this interface rather
than changing it.

**poll** — `watch [rules] -- CMD ARGS...`. Runs CMD on the interval and
compares successive snapshots.

- With structured output (`--json` natively, or through `bashy adapt --json --
  CMD` for a foreign command) it diffs **records** by key and emits `added`,
  `removed` and field-level `changed` events. It does not diff text lines.
- Without structured output it falls back to the classic line diff.
- That one source already covers `sprint show`, `mb --peek`, `weave list`,
  `resources system`, a model quota, `doctl`, `kubectl`, `ls -l` / `stat` (file
  mode changes) and `curl /version` — with zero per-command code.

**mb** — the host message board (`pkg/bus`): posts as they land, plus the
receipt levels below. Read through the board's own API (`Posts`, `SeenSeq`); no
new store.

**sprint-thread** — sprint thread entries. Every `weaveStoryAppend` already
writes `room.Record(EventSprint, author, "sprint:#N", kind, body)`, and that
room timeline is what `bashy events` reads. So the source is a reader of the
existing timeline (`room.Stream`), which means a watcher started late still
drains the entries it missed, and a stage change cannot reach the board without
reaching the watcher.

## Rules

One vocabulary, not one per command. A rule is:

```go
type Rule interface {
    Name() string
    On(ev Event, now time.Time) []Alert   // event-driven
    Tick(now time.Time) []Alert           // time-driven
}
```

`Tick` is the half a hand-written poll loop never has, and it is why absence
works at all: nothing arrives, so only the clock can fire.

- **match** — a predicate on the event: `field=status after=done`,
  `text=~regex`, a kind, a locator glob, an attr equality.
- **change** — any change of a selected field or key set.
- **inverse (`not ... within T`)** — the operator the story asks for, and the
  one that subsumes three rules. It fires when the positive condition has NOT
  held for T:
  - *idle*: no input and no output for T. Applies to a session's pty byte
    counters, a run log's stdout/stderr counters, a file's mtime, an
    interface's rx/tx, or a queue with no new records — they are all "activity
    on a locator". The policy it carries is the next section.
  - *liveness*: an expected heartbeat did not arrive within T. Locators named
    with `Expect` are armed at start, so a heartbeat that **never** arrives
    fires; otherwise a locator is armed when first seen.
  - *progress vs alive*: a locator that is alive but idle reports `stalled`.
    Process liveness is never evidence of progress, so the engine has no way to
    spell "healthy because the pid exists".
  - *un-stall*: activity resuming emits the `clear`, carrying the idle
    duration — and it clears **whichever rung was raised**, because nobody
    wants the critical cleared while the warning stays behind.
  - *absence* is this rule with the whole source as the locator.

### The idle policy

Four statements, in quarter-hour steps because the numbers have to be sayable:

| | |
| --- | --- |
| 15m quiet | **warning** (`level: warning`, reason `idle`) |
| 30m quiet | **critical** (`level: critical`, the line says *stalled*) |
| re-notify | every 15m while the condition persists |

Two rungs rather than one, because a single threshold either cries wolf at
thirty minutes of normal quiet or notices a dead run an hour late, and which
one it does depends on a number nobody can pick correctly for every task. The
level is part of the alert identity, so an escalation is **delivered** — a
dedupe that swallowed the critical would leave an operator looking at "idle"
for something that has been dead for an hour. It is a `level`, not a new
`reason`: a consumer filtering `reason=idle` keeps seeing the condition when it
escalates.

- **Idle applies to LONG-RUNNING work only** — weave runs, managed sessions,
  services, background and schedule jobs. A short foreground command that
  printed nothing for twenty minutes is not stalled, it is a command, and
  whoever started it is watching it. The engine cannot *guess* which one it is
  looking at and guessing from a locator string would be wrong in both
  directions, so the class is **declared**: by the source that spawned the work
  (`watch.Task`, delivered to the rules before the first cycle) or on the event
  itself (the `task` label). A locator declared `command`/`foreground` is never
  flagged; an undeclared one stays eligible, because the operator who pointed
  `--idle` at it has said what it is. `watch --task run|session|service|job|
  schedule|command` is the CLI spelling.
- **A task may declare its own expected quiet window** (`--quiet-window 45m`,
  the `quiet_window` label, or `Task.Quiet`). The declared window *replaces*
  the warning threshold and the critical rung keeps the rule's own ratio, so
  45m gives 45m/90m rather than collapsing both rungs onto the same instant.
  A window that does not parse leaves the default in force and is reported in
  the alert's `quiet_window_error` field — never silently ignored.
- **A task younger than its window is never flagged.** This is not the same as
  "we have not seen it for that long": a journal entry an hour old is evidence
  the task is *at least* that old, and a `Started` a source declares is the
  only fact that distinguishes a genuinely quiet run from a one-minute-old one
  with an hour of backfilled history. `Started` does not seed the clock — a
  watcher that has only just begun cannot claim the task was already stale
  before it looked.

`--idle`/`--heartbeat` without `--renotify` take the 15m cadence; a rule file
says `renotify` itself. A rule that wants one rung sets `"no_critical": true`.
- **correlation (request → response)** — a trigger, an expected event, a
  deadline, and a correlation key. Emits `responded` (with latency), `late` (a
  response after the deadline) or `unanswered` (at the deadline, naming the
  strongest level reached).
  - Levels are ordered and explicit, because **a queued message is never an
    acknowledgment**: `delivered` (it is on the board) → `read` (the
    recipient's cursor moved past it) → `replied` (a post back). Only the last
    level satisfies the rule; `unanswered` reports how far it got, so "B never
    looked" and "B read it and said nothing" are different alerts.
  - The same rule shape covers a steer and its ACK, a decision request and the
    operator's answer, a `bashy ask` prompt and its answer, a submitted story
    and the manager's accept/fail, and (Phase 2) an HTTP request span and its
    response span.
- **threshold** and **rate** are Phase 2 and are not implemented here. The
  `Rule` interface above is what they need and nothing more: a threshold
  evaluator is a rule that reads a numeric field off `sample` events and keeps
  hysteresis state in itself. `pkg/resources`' evaluator can move in behind
  that interface without the engine changing — which is the point of defining
  it now rather than after.

Rules compose with `any` / `all` over predicates; a rule set is flags for the
simple cases and a rule file for the rest. No new DSL: the file is JSON with a
`bashy-watch-rules-v1` envelope, the same shape the flags build.

## Alerts, dedupe and sinks

A rule emits `Alert{rule, state: fire|clear, reason, locator, message, fields,
event}`. The engine, not the rule, owns delivery policy:

- **transitions only** — an identical `fire` (same rule, locator and reason) is
  suppressed while the state persists;
- **re-notify** — unless `--renotify D` has elapsed, so a condition that
  matters is re-stated without a storm;
- **clear on recovery** — and only if the condition had actually fired, so
  nobody is told that something they were never warned about is now fine.

Sinks are one method (`Deliver(Alert) error`):

- **console** — one line per alert on stderr, so stdout stays the command's;
- **NDJSON** (`--json`) — one `bashy-watch-v1` object per line on stdout;
- **inbox** — a post to a recipient or seat through the board's own send path.
  The sink takes a `Board` interface, so tests inject a fake and **no test ever
  sends real mail**;
- **until** — exit on the first alert. This is the sink that replaces
  hand-written until-loops: an agent blocks on
  `watch --until --timeout 15m [rules] -- CMD`;
- **hook** — a Bash# script the user wrote. See the next section.

### Exit codes

| code | meaning |
| --- | --- |
| 0 | normal end: `--until` fired, `--chgexit` saw a change, or the cycle budget ran out |
| 1 | `--timeout` elapsed with no alert (the awaited condition did not happen) |
| 2 | usage error |
| 8 | `-e/--errexit`: the polled command exited non-zero (classic GNU) |
| 130 | interrupted |

1 vs 0 is the distinction a blocking agent needs: "it happened" and "time ran
out" must not be the same answer.

## Never "everything changed"

The replay failure is designed out rather than guarded against:

- a snapshot whose command **failed**, or which does not parse, does **not**
  advance the baseline and produces no diff events — it produces a source
  error, reported once;
- an **empty** snapshot against a non-empty baseline is treated the same way:
  the command told us nothing, which is not the same as "every record was
  removed". `--allow-empty` opts into reading it literally, for a command whose
  empty output genuinely means empty;
- a record with no key field is keyed by its canonical content, so an
  unkeyable stream degrades to `added`/`removed` rather than to a fake
  `changed`;
- all input is handled as bytes. Binary output is diffed byte-wise and
  **sanitized at render time**, so a NUL cannot truncate a line or an alert.

## Presets — a rod, not a fish

Presets are ordinary rule files, embedded as the baseline ring and overridable
by a local ring (`pkg/assetring`, the same precedence skills and fleet use):

```
$BASHY_WATCH_DIR/presets/<name>.json     most precise
$BASHY_HOME/watch/presets/<name>.json    a relocated bashy home
~/.bashy/watch/presets/<name>.json       the default local ring
(embedded)                               the shipped baseline
```

Phase 1 ships `sprint`, `agent-reply` and `idle`. They are not verbs: copy one,
edit it, and `--rules ./mine.json`. `bashy resources watch` stays a thin
documented alias for a `resources` preset (Phase 2) so there is exactly one
watch engine.

## Bash# script hooks — the same rod, for sources and sinks

Presets let a user configure the engine. Hooks let a user **extend** it, in
both directions, without rebuilding bashy:

- a **probe hook** is a source. It prints NDJSON samples on stdout —
  `{"resource":"gpu:0","field":"utilization","value":91,"unit":"percent"}` —
  and the engine applies its rules to them exactly as to a built-in sampler.
  A vendor GPU tool, an app metric, a model quota, a remote API, a queue
  depth. One `sample` event per line, plus a `changed` event when a value
  moves, so a change rule works over a user's probe with no threshold rule in
  sight. (Threshold and rate rules are still Phase 2; what a probe's values
  cross today is the inverse rule's warning/critical/clear ladder and the
  change rule.)
- an **action hook** is a sink. It runs on a transition — warning, critical,
  clear, idle, unanswered — so reclaiming a disk, pausing a run or paging a
  seat is a script in the asset ring rather than a verb in this binary.

The alternative is a verb per resource and a verb per response, which is the
shape this whole engine exists to collapse.

**The event goes on stdin and the argv is fixed.** One `bashy-watch-v1` JSON
object (an alert for an action; nothing for a probe) on stdin, and the script's
configured argv verbatim. No alert value is ever interpolated into a script or
its arguments, so there is no shell string for a locator containing a quote, a
newline or a `$(…)` to break out of. A hook that wants a field reads its stdin.

Discovery is the ring catalog again, one `.bsh` file per hook:

```
$BASHY_WATCH_DIR/hooks/<name>.bsh     most precise
$BASHY_HOME/watch/hooks/<name>.bsh    a relocated bashy home
~/.bashy/watch/hooks/<name>.bsh       the default local ring
(embedded)                            the shipped examples
```

A local script shadows a shipped one of the same name — `watch --list-hooks`
shows which won, and `watch --hook-source gpu > ~/.bashy/watch/hooks/mine.bsh`
is how a user starts from an example. The shipped two are **examples, not
verbs**: `reclaim-go-build` (orphaned `go-build*` temp dirs older than 15m with
no open handles, dry-run unless configured with `--apply`) and `gpu`
(nvidia-smi when present, and **nothing** when absent — "there is no GPU here"
is not a probe failure).

### Safety

Applied in one place, `Hooks.Run`, so no caller can forget one:

- **per-hook timeout** — 60s for a probe, 5m for an action;
- **concurrency 1 per hook** — an overlapping invocation is *skipped*, never
  queued, because a queue of reclaims is a reclaim storm with a delay;
- **a non-zero exit or a timeout is itself an alarm event**, raised into the
  alert stream (reason `error`, rule `hook:<name>`) through the engine's own
  dedupe. A hook that quietly did not run leaves the operator believing it did.
  A probe that failed produces that alarm and **no events**, for the same
  reason a failed snapshot does not advance a baseline. One unreadable sample
  line does not discard the readable ones and is not dropped in silence either;
- **A4 — the master never runs user hooks.** There is no master/worker role
  check in this module to reuse, so the refusal is driven from an explicit
  signal: `BASHY_WATCH_ROLE=master` in the environment (or `Hooks.Master` in
  code). The master's job is to decide; running a user's script is a worker's;
- **A6 — a destructive hook needs explicit confirmation** in its configuration
  (`"destructive": true, "confirm": true` on the sink), never a crossed
  threshold. Hooks otherwise run as the invoking user under the normal bashy
  policy;
- a hook that cannot be resolved is an **error**, never a hook that does
  nothing: a watch whose sink silently never ran reports a problem and then
  ignores it.

Execution is `bashy --bashsharp <script> <fixed argv>` through the **installed
bashy, looked up on PATH at run time** — deliberately not `os.Executable()`,
which in a test or an embedding host is something other than a shell. The exec
itself is injectable (`Hooks.Exec`), so unit tests exercise the policy without
a real bashy.

### Rule-file spelling

```json
{
  "schema_version": "bashy-watch-rules-v1",
  "sources": [{"type": "hook", "script": "gpu", "locator": "gpu:0", "class": "service"}],
  "rules":   [{"type": "change", "fields": ["utilization"]}],
  "sinks":   [{"type": "hook", "script": "reclaim-go-build", "args": ["--apply"],
               "destructive": true, "confirm": true,
               "when": {"attrs": {"level": "critical"}}}]
}
```

`sinks` is the only sink a rule file may declare: console, NDJSON and inbox are
the invocation's business, not a rule file's. `--probe NAME` and `--hook NAME`
are the flag spellings for the simple cases; a fixed argv, a `when` filter or a
destructive hook needs the file.

## Classic watch is untouched

No agentic flag given ⇒ the classic path runs, byte for byte: `-n`, `-d`,
`-g/--chgexit`, `-e`, `-t`, `-x`, the header, the reverse-video diff and the
GNU exit codes. The agentic mode is additive and is selected by the presence of
an agentic flag; the existing tests are the gate on that.

## Phase 2 (deferred)

OTel spool source (`spans.jsonl`: requests, responses, errors and metrics are
already spans, so there is no new instrumentation API), fsnotify source
(create/write/remove/chmod — file mode changes), weave run-state source,
threshold + rate rules shared with `pkg/resources`, wiring `pkg/resources`
onto this engine, and the `deploy` and `resources` presets. (The hook sink
moved into Phase 1 — see above.)
