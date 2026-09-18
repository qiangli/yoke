# Foreman context contract: bounded continuation + sequenced state changes

**Status:** shipped 2026-08-29 (the Foreman slice of the coordination
context-efficiency plan, `dhnt/docs/bashy-coordination-context-efficiency-plan.md`
§5.4 / Phase 2 + 3).

Foreman puts material in front of an agent at two seams — the prompt of a
non-steerable (replay) turn or a live session's opening prompt, and the packet
of a DAG target — and puts material in front of a *supervisor* at one:
`foreman status`. This document is the contract at those seams. Nothing here
adds a store of truth; `state.json` and the session directory remain the
record.

## Truth, delta, context

| Representation | Where | What |
|---|---|---|
| Truth | `<session>/state.json` | the session state, with `seq` + `digest` |
| Truth (artifact) | `<session>/history.jsonl` | every turn, verbatim, chained digests (`bashy-foreman-history-v1`) |
| Delta | `<session>/transitions` | one `bashy-foreman-transition-v1` line per committed state change |
| Context | the prompt | goal + kb preamble + **checkpoint + recent window** + the new message |

The whole-history replay ("Session history:" followed by every turn) is gone
from both prompt paths. `chat.Session` steering is unchanged: a live agent
still receives the raw message mid-turn; only the opening prompt is composed.

## The checkpoint (`bashy-foreman-checkpoint-v1`)

`Session.Checkpoint()` is a projection — labelled as one in the prompt — of:

- goal, status, current step, state `seq`/`digest`;
- **accepted decisions**: the operator's last `MaxDecisions` (5) instructions
  (`tell` and mid-turn steers), each previewed to `DecisionPreviewBytes` (256),
  with the total count;
- **blockers**: the state's `blocker` (why it is blocked — pause, runner exit,
  unreachable live agent, artifact write failure), plus stop reason;
- **last agent result**, previewed to `LastResultBytes` (1024), with its
  history seq and full byte count;
- **recent window**: the last `RecentWindow` (6) turns, each previewed to
  `EntryPreviewBytes` (512);
- **history reference**: artifact path, entry count, byte count, and the
  chain digest of the last entry, which fingerprints the whole history.

The rendered section is capped at `ContinuationBudget` (8 KiB) *after*
rendering: the recent window shrinks first, then the last result is dropped,
then the text is cut. References and blockers survive. Every preview is a
UTF-8 byte bound that never splits a rune and states the exact omitted count
(` …[+N bytes]`).

Consequence (pinned by `TestPromptReachesSteadyCeilingOverLongHistory`): a
250-turn session hands its agent a prompt of the same size as a 120-turn
session. Prompt size is `goal + kb + message + ≤ 8 KiB`, on every turn.

DAG targets get the same checkpoint plus **dependency outputs by reference**:
for each `Requires:` target, a 256-byte preview with the history seq and byte
count of the verbatim result. Predecessor conversations are never
concatenated.

### Retrieval

The history artifact is for audit and explicit retrieval, never for prompts:

```
jq 'select(.seq==81)' ~/.bashy/foreman/<id>/history.jsonl     # one verbatim turn
```

`Store.LoadHistory()` is the programmatic form. The in-memory projection is
rebuilt from the artifact on `Open`, so a restarted daemon composes the same
checkpoint the previous one would have.

## State changes (`bashy-foreman-transition-v1`)

`Store.Commit` is the one write path for `state.json`:

- `CanonicalDigest(state)` hashes the state minus `updated_at`/`seq`/`digest`.
- Identical digest → **nothing is written**: no file change, no journal line,
  same `seq`. An unchanged healthy session is silent at every layer.
- Different digest → `seq = max(state seq, journal tail seq) + 1`, atomic
  state replace, then one journal append.

The journal is a delta view, not a second truth. `Store.Changes(after)`
returns every transition with `seq > after`; if `state.json` is ahead of the
journal (crash between the two writes, or a pre-contract session with no
journal) the missing head is synthesized from the state and nothing is
written. A legacy `state.json` without a seq is seq 1, and the next commit is
seq 2, so a cursor taken before the contract never collides.

Transitions emitted by the lifecycle are exact
(`TestExactStateTransitionsAreSequenced`):

```
- → idle → working → idle → blocked(paused by operator) → idle → done
```

DAG runs persist `idle → working(target) → idle` per target.

### Wait / read

`Store.WaitChanges(ctx, after, bound)` returns as soon as a transition after
the cursor exists, or `nil, nil` when the bound elapses. It stat-polls
`state.json` and the journal every `DefaultWaitInterval` (100 ms) and parses
only when a size/mtime moved; file notification would be an optimization, the
rescan is the correctness floor.

CLI (`cmds/foreman`, the status adapter):

```
foreman status <id>                      one bounded snapshot; --json carries seq + digest
foreman status --after SEQ <id>          the transitions after the cursor, now
foreman status --wait 30s <id>           block for the NEXT change (or --after SEQ); timeout = no output, exit 0
foreman status --watch --json <id>       NDJSON, one record per change, until the context ends
```

Human rows are `id  seq  prev->status  step  blocker`. JSON stdout carries
only versioned records. An unknown session is an error, not a silent wait.

## Not in this slice

- The per-turn call/output budget around autonomous repair loops (§5.4, last
  bullet).
- Extracting the stat/rescan/digest wait loop into the shared primitive the
  plan names for inbox/Bus/Meet/Weave; `pkg/bus` still has its own loops.
  When that primitive lands, `Store.WaitChanges` is the adapter to port.
- Bashy's `agents`/room views do not yet consume `Store.Changes`; the
  `bashy foreman` verb reaches this through `cmds/foreman` unchanged.
