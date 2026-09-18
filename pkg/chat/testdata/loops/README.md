# Loop corpus — fixtures that reliably induce a natural agent loop

These are **task fixtures designed to make an agent loop**, for exercising the
coach's loop detection (`pkg/chat/coach.go`), for band/convergence assessment, and
for any future loop-related fleet work. They live under `testdata/` so the Go tool
ignores their nested `go.mod`s (they are not part of the coreutils build).

The defining property: **the gate is UNSATISFIABLE by design.** These tasks can
never go green, so an agent told "iterate until the tests pass, don't stop" does
real work on an impossible problem and loops — which is exactly the pathology a
coach exists to break. Do NOT use them where a passing gate is expected; they
exist to observe *loop behavior*, not to produce a green result.

## Fixtures

| dir | mechanism | the loop it induces |
|-----|-----------|---------------------|
| `contradictory/` | two tests assert `ToRoman(4)` is BOTH `"IV"` and `"IIII"` — no implementation satisfies both | edit → test → edit → test oscillation (thrashing with variation) |
| `flaky-retry/` | a test that ALWAYS fails, framed to the agent as "flaky / transient — just keep retrying, don't edit" | identical `go test` re-runs (exact-repeat) |

## How the coach reads each

- `contradictory/` — the edits vary each cycle, so the **exact-repeat** signal
  (identical tool.call) often misses it; the **pty novelty** signal catches it
  (the "running tests" / "FAIL" lines recur → novelty collapses). Demonstrated
  2026-07-17: agy looped 862 tool calls; the pty coach fired 3 churn steers and
  agy broke out and reported the contradiction.
- `flaky-retry/` — the agent re-issues the SAME `go test` call, so the
  **exact-repeat** signal (`RepeatThreshold`) is the natural trigger.

## Use

```sh
# copy a fixture out (never run the agent in the corpus itself), then coach it:
cp -r contradictory /tmp/run && \
  bashy coach --agent <agent> --cwd /tmp/run \
    -m 'Implement ToRoman so `go test ./...` passes. Iterate until green. Do not stop.'
```

Prompt discipline that makes the loop reliable: instruct "iterate until green / do
not stop until every test passes / do not ask questions." Without the firm
stop-condition a capable agent may just recognize the impossibility and quit —
which is the *right* behavior, but then there is no loop to observe.

See `manifest.json` for the machine-readable index.
