# Request progress: what a sender sees while an agent works

Design of record for sprint 126, story #224 (probe) → #216 (M3 fixes).
Scope: `pkg/meet` + `pkg/webconsole`. OSS-safe; no cloudbox internals.

## The requirement

When a sender — a human in the web UI, or another agent — hands a request to an
agent, they must be able to tell, cheaply:

1. the message was accepted,
2. work is under way,
3. it is still moving (not wedged),
4. roughly what it is doing,
5. it finished, or failed.

"Cheaply" is the load-bearing word: **no model tokens, minimal network**. A
progress mechanism that costs a turn to ask "how's it going" is worse than none —
it spends the budget it is reporting on, and it perturbs the thing it measures.

The failure it exists to prevent is a person staring at a screen for minutes,
unsure whether the message was even received.

## What already exists (measured 2026-09-06, from source)

Most of this is built. Recording it so nobody rebuilds it.

**Two channels, one truth** (`pkg/meet/live.go`):

| file | role | granularity | durable? |
|---|---|---|---|
| `transcript.jsonl` | the RECORD — one sanitized event per completed turn | turn | yes; minutes are built from it |
| `live.jsonl` | the VIEW — a tee of the agent's stdout | line | no; ephemeral, safe to lose |

The live channel cannot show a watcher anything the record will not also
contain, because it is a tee — so observing never changes what is recorded.
Granularity is a LINE, not a token, and that is a deliberate honest limit: the
agent CLIs bashy drives emit whole lines, and subscribing to tokens would mean
going around the harness straight to a provider API, abandoning its tools,
sandbox and shell-forcing.

**The state vocabulary already exists**: `speaking` (took the floor) → `line`
(one line of output) → `spoke` (finished; carries `Status`).

**The counters already exist**: `LiveEvent` carries `Lines`, `Bytes` and
`ElapsedMS`.

**The sampling is already cheap by construction** (`dmProgressSampler`,
`pkg/meet/relay_dm.go`): frames are emitted on FIBONACCI line numbers — 1, 1, 2,
3, 5, 8, 13, 21 … — so report density falls as the answer grows and the total
frame count is logarithmic in output size. Each frame carries a length-capped
snippet; a heartbeat fires when a turn goes quiet. Cost in model tokens: zero. It
is a projection over bytes the agent was already writing.

**The UI already consumes it**: the shipped SPA references `/observe`,
`/observe-dm`, `elapsed_ms`, `speaking`, `spoke`, `line`.

## The gaps

- **G1 — rooms get no progress.** `live.go` states the counters are "populated by
  the DM observer's bounded progress projection … and therefore remain absent
  from ordinary meeting live events." A DM shows progress; a ROOM does not. Rooms
  are where sprint work is discussed.
- **G2 — a job can be cancelled but not asked about.** The long verbs (`ask`,
  `round`, `poll`, `address`, `converge`) return 202 + `JobRef{job,room}`.
  `pkg/meet/recall.go` already tracks `committed` / `recalled` / `finished` per
  job, server-side. The only verb on that id is `recall` (cancel) — there is no
  status read path. A sender holding a job id cannot ask whether it is running,
  which is the agent half of the requirement, and the state to answer it is
  already in memory.
- **G3 — unverified in a browser.** Everything above is source, not behaviour.
  Whether any of it reaches the screen is exactly M3's open question.
- **G4 — delivery and execution are different axes.** Sprint 126 binds the
  delivery vocabulary (`accepted, queued, delivered, read, failed, unverified`)
  and it **ends at `read`**. Everything in this document is about what happens
  *after* `read`. Collapsing the two would let "delivered" read as "being worked
  on", which is the confusion the requirement is trying to remove.

## The rule: progress is OBSERVED, never REPORTED

An agent must not be asked to describe its own progress. Asking costs a turn,
pollutes its context, and yields a claim rather than a measurement — and this
fleet's own evidence says self-reports are the least trustworthy signal
available (all three harnesses exited 0 while failing).

Everything here is therefore derived from bytes that already exist: stdout lines
the agent wrote anyway, timestamps, and server-side job state already held in
memory. That is what makes it free.

## No percentage — and why that is the honest answer

The requirement asks for "percentage / percent remaining". **A single agent turn
has no known total**: nothing knows how many lines an answer will be, so any
percentage would be fabricated, and a progress bar that invents its own
denominator is the same class of confidently-wrong signal this codebase refuses
everywhere else.

The honest substitutes answer the real question — *is it alive and moving?* —
and all four already exist:

| signal | answers |
|---|---|
| elapsed | how long it has been working |
| cumulative lines / bytes | that it is producing, and how much |
| heartbeat | that it is alive during a quiet stretch |
| last-line snippet | what it is doing right now |

**Where a total genuinely is known, a percentage is real and should be shown** —
weave run points, stories closed of N, gate steps completed. That is a different
measurement and must not be generalized back onto a single turn.

## Agent-to-agent: the same mechanism, and the cost is the whole point

This is not primarily a UI feature. The same question — *did my request land, is
it moving, is it done* — is what one bashy agent must ask about work it delegated
to another, and there the cost is not cosmetic: every coordination message a
recipient reads is context it pays for, in tokens, on every turn thereafter.

**The measured warning is already on file.** `../docs/agent-comms-retention.md` (umbrella)
measured a real board: 54% of it had been written that same day, so a one-day
retention window still cost ~19k tokens against ~34k for the entire history.
The conclusion transfers directly — *retention by age cannot fix a token problem
caused by today's volume*. Coordination gets cheaper by projecting better, never
by sending more and trimming later.

### Pull is free; push is expensive. Progress is PULL.

| mode | cost to the reader | right for |
|---|---|---|
| PULL — a projection on disk, read when the agent chooses | ~zero; nothing enters context until asked, and only the answer does | progress, status, "where is it now" |
| PUSH — a message into an inbox | every arrival is context the recipient pays for, forever | a state CHANGE the recipient declared it needs to act on |

So: **an agent monitoring delegated work polls a projection at its own turn
boundary. It does not receive a message per progress frame.** A progress channel
implemented as messages would bill every watcher for every increment — N agents ×
M frames — which is precisely the overwhelm to avoid.

Push stays for genuine state CHANGES (submitted, failed, blocked, needs a
decision), and it already has the machinery: `pkg/bus`'s sidecar holds the
subscription, matches topics and applies governance and rate rules OFF the
agent's critical path, leaving a pre-resolved buffer to read at a turn boundary —
under **demote, never drop**.

### Why cumulative counters are the cheap shape

`Lines`, `Bytes`, `ElapsedMS` are **cumulative, not deltas**, and that is what
makes a poll cheap:

- **One read gives the whole state.** A counter is idempotent — the reader needs
  the latest value only. A stream of deltas has to be replayed from a cursor to
  mean anything, so a watcher that missed frames must fetch and pay for all of
  them.
- **Missing frames costs nothing.** The Fibonacci sampler can drop density as
  output grows precisely because no single frame carries irreplaceable state.
- **The answer is bounded.** "Where is it now" is one small object per task,
  whatever happened in between — so a manager polling ten delegated runs pays for
  ten lines, not ten transcripts.

This is the same rule `pkg/foreman` already follows: state changes are sequenced
and **digested** for `status --wait`, and its prompts carry a bounded checkpoint
plus a recent window, never the whole history
(`docs/foreman-context-contract.md`, in this repo).

### The shape a coordinating agent should get

One bounded line per delegated task, answering only what a routing decision
needs:

```
task            state       elapsed   produced      last
weave/3         working     4m12s     318 lines     running the gate
weave/4         done        7m52s     1 commit      submitted
meet/ask-9f2    working     0m31s     12 lines      reading pkg/meet
meet/ask-a03    QUIET       6m04s     0 lines       no output since start
```

`QUIET` is the line that earns the feature — alive but producing nothing is the
failure a manager currently catches last, and it is derivable for free from a
heartbeat plus a flat counter. It is the same signal `sprint tick` reports as
`silent` for weave runs; this extends it to in-flight conversations.

### Rules

1. **Progress is polled, never mailed.** No progress frame becomes an inbox item.
2. **One digest per task, not a stream.** The reader asks "where is it now".
3. **Cumulative counters, so a missed frame costs nothing.**
4. **Push only what needs a decision** — and through the existing sidecar, under
   its rate rules, demote-never-drop.
5. **No model tokens on the producing side, ever.** Everything is a projection
   over bytes already written.

## Constraint on any fix

`JobRef`'s own doc is binding:

> There is deliberately no second progress channel: a job that reported itself
> somewhere else would be a second truth about what happened in the room.

So a fix EXTENDS the existing live channel and the existing job record. No new
store, no new transport, no second channel, no new panel.

## Sequence

1. **#224 (probe)** — record the symptoms with a red `verifydom` case each;
   confirm or refute G1/G2/G4 against the code. Ships no fix.
2. **#216 (M3)** — fix what #224 proved, using the existing gate.

A UI bug fixed without a failing browser case is indistinguishable from one that
was never there.

## MVP probe result — 2026-09-05

For Sprint 126's narrowed human-interaction and sprint-management MVP, the
existing path is coherent and no M3 production defect was reproduced:

- **G1 confirmed, deferred.** Ordinary multi-party room frames omit the bounded
  counters that Chat/DM frames carry. The MVP uses the existing one-to-one Chat
  path for human-to-agent work; extending room progress is a later feature.
- **G2 confirmed, deferred.** A `JobRef` can be recalled but has no separate
  status-read verb. The human MVP observes the existing live/transcript stream;
  adding a job-status surface is not required.
- **G3 passed.** The Meet Playwright suite drives a deterministic agent through
  the real HTTP, process-launch, WebSocket, transcript, and rendered-DOM path.
  It proves accepted work, a visible working indicator, bounded cumulative
  progress, final response rendering, and visible failure. A new regression
  case also proves two agent Chats do not leak messages into each other. The
  full suite passes 29/29. The console `verifydom` suite passes independently.
- **G4 confirmed.** Delivery remains the six-state bus receipt rendered by the
  Messages app; execution remains the Meet/Chat live and transcript state. The
  integration gate proves both without collapsing them.

The command-level gate in `script/e2e-sprint-modes.sh` passed 22/22 with a
freshly built Bashy binary, but two assertions in that run were subsequently
declared obsolete: a person must not own a production sprint seat or hold its
manager watch. Sprint 127 story #237 owns that correction. The supported MVP
subset passed: a human principal can send durable MB and Meet instructions,
observe truthful delivery, and steer a registered agent managing a sprint. The
production defect found on that path was the managed-owner control-socket
readiness race, fixed as Sprint 127 story #235.

Therefore #216 has no evidenced MVP fix to make. It remains a valid container
for future operator-reproduced UI defects, but is not a Sprint 126 deliverable.
