---
id: 5e01de0e5322
kind: bug
title: chat inbox prepend floods a new agent with the whole mb backlog every turn (coordination overflow never drains)
seq: 3131
status: todo
priority: p2
labels:
    - agents
created: 2026-11-19T21:41:50.993628Z
---

Observed 2026-11-19 on a live interactive session: bashy chat --agent agy-gemini4-carbon -i (a binding with no prior inbox cursor). Every turn's prepended '## Coordination input' carried a bashy-coordination-overflow-v1 envelope covering the whole message-board history (source mb, priority 4, ~779-781 items, ~333 KB; first_id 2276/10272 .. last_id 14711; 'unrepresented_items' == omitted_items, referenced_items 0, open: 'bashy inbox --as "agy-gemini4-carbon" --peek'). The agent spent each turn acknowledging historical posts ('historical posts; no action required') and detoured into an orphaned mb-rotate lock it found there; the operator had to interrupt (chat interrupt) to get it onto its task. Two defects suspected (root cause NOT yet confirmed — investigate before fixing): (1) a subscriber with no cursor starts at the beginning of history instead of 'now' (+ messages addressed to it/its role); (2) items shown only as overflow are not advanced, so the same backlog is re-sent every turn. Benchmarks avoid it via BASHY_CHAT_INBOX=off. Code: pkg/bus (Prepend in chat.Session.Say), pkg/room (cursors). Acceptance: a fresh binding's first turn carries only addressed/recent items; a shown overflow advances the watermark (DEMOTE, NEVER DROP still holds — the backlog stays readable via bashy inbox --peek); unit tests for both.

Operator requirement (2026-11-19): cap the per-turn prepend by default — show at most ONE message (highest priority, then newest; messages addressed to this agent/role first) plus one indicator line with the totals, e.g. '1 of 781 unread (3 addressed to you) — read more: bashy inbox --as <id>'. The cap is the default and configurable (e.g. BASHY_CHAT_INBOX_MAX=N; BASHY_CHAT_INBOX=off unchanged). Nothing is dropped: the rest stays readable and counted. Acceptance adds: default prepend size is bounded (1 item + counter) regardless of backlog size; unit test with a large backlog.
