# Meet room reaping — `bashy meet abandon` and empty/foreign-repo close guards

`meet` had exactly one exit — `close` — and it means **conclude properly**. That
is the right ceremony for a live meeting reaching its end and the wrong one for a
room that has been dead for six weeks. This note records the two defects that
followed from having only one exit and the changes that fix them.

## Defect 1 — the only exit is "conclude"

`close` runs three steps, each correct for a concluding meeting and none correct
for reaping a stale room:

1. **converge** — SPAWNS the secretary to synthesize decisions. Not skipped by
   `--yes`.
2. **confirmConclusion** — SPAWNS/prompts the initiator to confirm the end.
   Skipped by `--yes`.
3. **fileMinutes** — writes the minutes document.

For a room nobody will ever conclude there is nothing to synthesize, nobody to
confirm to, and no repo that wants its minutes. Reaching for `close --yes`
suppresses only step 2 — the expensive secretary pass in step 1 still runs.

### Fix — `bashy meet abandon <ref>`

A janitorial exit modelled on the verb `weave` already had. It:

- marks the room **`abandoned`** (a status distinct from `closed`, so a reaped
  room never reads like a concluded one afterwards),
- **releases the room number** for reuse,
- **archives the transcript** beside the room's other artifacts, through the same
  `archiveSessionArtifacts` path a reopen uses — so the transcript **survives**;
  this is **not a delete**, and
- **spawns nothing, synthesizes nothing, files nothing.**

It records one plain `abandoned` marker into the transcript (no agent launched)
before archiving, so the archived record carries *why* the room ended. Because
abandon exists, there is no longer a reason to reach for `close --yes` on a dead
room.

## Defect 2 — `fileMinutes` always writes, into a repo captured at OPEN time

`fileMinutes` wrote unconditionally, and resolved its destination from `st.Cwd`,
captured when the room was **opened**. Two consequences:

- A room with **zero turns** produced ~1.2 KB of NOT-EXTRACTED boilerplate.
- Closing a room today wrote into whatever repo someone stood in **weeks ago**.
  Measured: one close landed a note in `bashy/docs/meetings/`, dirtying a
  submodule during an active certification campaign, from a room opened three
  weeks earlier.

### Fix

- **Skip filing when a room has no fileable content** (no turn, vote, human post,
  or decision/action/note marker). The room still closes and releases its number;
  the transcript stays in the store. Board posts count as content (they record as
  `human`), so an active board is never skipped.
- **Resolve the destination against the caller's repo at close time.** If the
  minutes would land inside the repo the room was opened in, and that is not the
  repo the caller is standing in now, **refuse and name the path** rather than
  write into someone else's tree.

The **NOT-EXTRACTED** notice itself is kept — it is deliberate and correct for a
secretary-less room that *did* have turns.

## Tests

`pkg/meet/reaping_test.go` covers both defects, including a `spawnGuard`
`chat.Runner` wired in as the api runner that **fails the test if invoked**,
proving `abandon` spawns nothing. Existing close/amend tests were updated to close
from within the repo the room was opened in, reflecting the new destination
contract.
