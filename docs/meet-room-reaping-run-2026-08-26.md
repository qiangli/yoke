# Meet room reaping — operational run, 2026-08-26 (issue 29)

Operational task, not a feature. It landed after issue 13 shipped
`bashy meet abandon` (commit `2a40df6`, merged `f024182`), which is the verb it
needs: `abandon` marks a room ABANDONED, releases its number, archives the
transcript, and **spawns nothing** — see [meet-room-reaping.md](meet-room-reaping.md).

## Why not `close`

Eight rooms were open and idle for 12–48 days, each holding a low room number
that the lowest-free-among-open rule (`pkg/meet/room.go`) hands to every new
room. Every one HAS a secretary, so `close` would run `converge` and SPAWN that
agent to synthesize a 30-to-48-day-old dead discussion — real token spend on
rooms nobody will read. One (rm3's, secretary AIDER) is retired from the fleet,
so a `close` would try to spawn an agent with no binding and likely fail
partway. `close --yes` was **not** used as a workaround: `--yes` suppresses the
confirmation prompt but not the expensive secretary pass.

## What was reaped

Seven rooms abandoned by ID (IDs, not room numbers — numbers shift as rooms
release, and rms 1/2 shared a topic):

| Freed room | ID | Idle |
|---|---|---|
| 5 | `2026-07-08-sanitize-regression-check-4a0f` | 48.3d |
| 4 | `2026-07-08-sanitize-fix-v2-check-ddfe` | 48.3d |
| 3 | `2026-07-11-bashy-platform-retrospective-fleet-testing-block-c699` | 45.0d |
| 1 | `2026-07-14-ycode-is-2-8x-slower-than-opencode-on-the-same-m-c10c` | 42.8d |
| 2 | `2026-07-14-ycode-is-2-8x-slower-than-opencode-on-the-same-m-69b2` | 41.7d |
| 6 | `2026-07-18-in-2-3-sentences-each-what-is-the-single-highest-6cb0` | 38.8d |
| 7 | `2026-07-24-finalize-bash-exact-go-contextual-syntax-and-imp-543f` | 32.6d |

**LEFT OPEN — rm16** `2026-08-13-posix-certification-campaign-9347` (12.9d): the
POSIX certification campaign room, only 12.9 days old. Untouched pending its
owner.

## How it was run

The installed `bashy` binary (`7e50b79`) predates the `abandon` verb, so a
throwaway `main` calling `meet.NewMeetCmd()` was built from this branch's source
(which includes issue 13), run against the same store (`~/.bashy/meet`), and
deleted. No source in this repo was changed by the run.

## Verification

- `meet list` — only rm16 remains open; the seven show status `abandoned` with
  no room number.
- Transcripts survive: each room's store dir is intact and its transcript is
  archived under `archive/<ts>/` (turns + `transcript.jsonl` + `live.jsonl`),
  readable. Not a delete.
- Nothing spawned: the last transcript record is a plain
  `{"kind":"abandoned","text":"room abandoned; not concluded"}` marker, no agent
  turn; the agent-process count did not rise (28 → 25), and no tokens were spent.
- No repo dirtied: `abandon` never calls `fileMinutes`, so no minutes note was
  written into any `docs/meetings/` tree (the failure mode a `close` on a
  weeks-old room caused before).
