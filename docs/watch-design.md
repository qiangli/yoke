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
  - *idle*: no input and no output for T (default 14m). Applies to a session's
    pty byte counters, a run log's stdout/stderr counters, a file's mtime, an
    interface's rx/tx, or a queue with no new records — they are all "activity
    on a locator".
  - *liveness*: an expected heartbeat did not arrive within T. Locators named
    with `Expect` are armed at start, so a heartbeat that **never** arrives
    fires; otherwise a locator is armed when first seen.
  - *progress vs alive*: a locator that is alive but idle reports `stalled`.
    Process liveness is never evidence of progress, so the engine has no way to
    spell "healthy because the pid exists".
  - *un-stall*: activity resuming emits the `clear`, carrying the idle
    duration.
  - *absence* is this rule with the whole source as the locator.
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
  `watch --until --timeout 15m [rules] -- CMD`.

Hook-a-command is Phase 2.

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

## Classic watch is untouched

No agentic flag given ⇒ the classic path runs, byte for byte: `-n`, `-d`,
`-g/--chgexit`, `-e`, `-t`, `-x`, the header, the reverse-video diff and the
GNU exit codes. The agentic mode is additive and is selected by the presence of
an agentic flag; the existing tests are the gate on that.

## Phase 2 (deferred)

OTel spool source (`spans.jsonl`: requests, responses, errors and metrics are
already spans, so there is no new instrumentation API), fsnotify source
(create/write/remove/chmod — file mode changes), weave run-state source,
threshold + rate rules shared with `pkg/resources`, the hook sink, and the
`deploy` and `resources` presets.
