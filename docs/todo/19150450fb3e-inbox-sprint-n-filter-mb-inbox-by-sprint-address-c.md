---
id: 19150450fb3e
kind: enhancement
title: 'inbox --sprint N: filter mb/inbox by sprint (address conductor:N, meet room, uuid, story ids, Sprint N / S<N> text); sprint show unanswered count uses it since sprint start'
seq: 1902
status: todo
labels:
    - bashy
created: 2026-10-11T09:50:58.790898Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Operator 2026-10-11. Problem: a manager seat reusing a binding sees its whole stale backlog; sprint show 419 flagged 15 unanswered for claude-opus5.5, all about sprints 319/406/110/408/114/314, none 419. --search 419 is too loose (ids, timestamps). Shape: (1) bashy inbox --sprint N|uuid (also bashy mb) resolves the card and keeps a message if addressed to conductor:N or its meet room, carries the sprint uuid or a story id/number, or says Sprint N, Sprint #N or S<N>. (2) Title keywords narrow via the existing --search, combined with --sprint; never select alone. (3) sprint show N UNREACHABLE count applies the same filter from the sprint start. Keeps identity choice free: fresh s<N>-manager is clean by default; a manager reusing the original identity for context scopes with --sprint.

Current operator disposition 2026-10-11: this is a separate required delivery story in Sprint 418. The earlier move-out and absorption directions were superseded before implementation; their history remains in the Sprint 418 evidence. Keep the operator acceptance above intact. Implement the smallest shared sprint relevance rule for the existing inbox/mb reader and `sprint show` unanswered count, reusing the accepted safe reader semantics of todo:f96c615163fc. No second inbox or transport.
