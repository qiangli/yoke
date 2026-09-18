# Sprint lifecycle

`bashy sprint` separates the lifecycle of a cross-repository initiative from
the lifecycle of its individual `weave` workers.

```text
start ──> running ──> pause ──> resume ──> running ──> end ──> done
                    (handoff)    (pickup)                (drain + gate)
```

## Common workflow

```sh
bashy sprint start 45 --owner current-manager --for 4h
bashy sprint pause 45 -m "current result; next action; blocker"
bashy sprint resume 45 --owner next-conductor
bashy sprint end 45 --gate 'go test ./...'
```

- `start` claims the conductor lease and opens a time box.
- `pause` requires a continuity note, releases the conductor lease, and leaves
  linked workers untouched. It is a conductor transition, not a worker-state
  transition.
- `resume` claims the lease and prints the saved continuity note immediately.
  It does not restart workers because `pause` did not stop them.
- `end` is deliberately strict. It parks linked working agents into resumable
  `paused` states, rejects missing or half-allocated runs, checks linked
  repositories are committed/pushed/pinned, requires a passing gate, closes
  the time box, releases the lease, and moves the card to `done`.

`end` has no `--force` or `--no-verify`: an ended sprint is an audited terminal
state, not merely a clock that stopped. Use `sprint stop` when closing only the
current time box, including an explicitly recorded unverified stop.

## Compatibility vocabulary

The lower-level verbs remain available:

- `handoff` is the primitive beneath `pause`; unlike `pause`, its note remains
  optional for compatibility.
- `take` is the primitive pickup operation; `resume` adds immediate continuity
  display.
- `stop` closes one cadence cycle and does not move the card to `done`.
- `abort` is the emergency teardown path and is not a normal lifecycle step.

The board is the quick machine-global continuity record. A detailed recovery
note may still live in the relevant repository and should be referenced from
the sprint continuity message.
