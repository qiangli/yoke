---
id: 96b06a4f2fb4
kind: bug
title: 'board: a sprint whose story_roots are missing on disk silently shows no stories'
seq: 3065
status: todo
priority: p2
labels:
    - app
created: 2026-11-09T01:01:33.145541Z
---

Problem
- yoke/pkg/board/sources.go todoSource.Load filters each sprint's stories to the stores named in that card's story_roots (`if roots := storyStores[x.Sprint]; len(roots) > 0 && !roots[sc.root] { continue }`).
- When a registered root no longer exists (checkout archived/moved/removed), `todo list --base-dir ROOT` fails, that store lands in `unreadable`, and the per-sprint filter still excludes the same stories found in every OTHER store (e.g. the repo they migrated to). Result: the web board (/sprint/) and `board` render the card with zero or partial stories. The only signal is one generic "N of M store(s) not read" warning, not tied to the card.
- Observed 2026-11-08 on the dev box: ~750 story_roots pointed at an archived superproject checkout after the move to the public umbrella; with that checkout absent, many sprint cards showed no stories. Worked around by re-cloning, then repointing the roots in ~/.bashy/sprint/queue.json (data-only; backup kept). The code path is unchanged and will recur on the next move.

Expected behavior
1. A story_root whose docs/todo store is missing is not authoritative: if ALL of a sprint's roots are unreadable, do not filter that sprint's stories by store (fall back to every reachable store, de-duplicated by story id, newest `updated` wins, so run-checkout snapshots don't resurrect closed stories).
2. If only SOME roots are unreadable, keep filtering to the readable ones plus fall back for story ids not found in any readable root.
3. Surface it per card: a sprint with an unreadable root carries a warning naming the sprint and the missing root (board JSON `warnings` + card attr), so the web board can show "stories store missing: <root>" on the card instead of an empty card.

Verification
- Unit test in pkg/board: sprint with story_roots=[missing dir], story with its sprint_id in another readable store -> story appears on the card, warning names the sprint + root.
- Existing snapshot-dedup test (closed story in a run checkout not resurrected) stays green.
- go test ./pkg/board ./pkg/webconsole (targeted; no full gates).

Optional follow-up (separate story if wanted): a `bashy sprint` verb to repoint/prune story_roots so a checkout move is a recorded edit, not a hand edit of queue.json.
