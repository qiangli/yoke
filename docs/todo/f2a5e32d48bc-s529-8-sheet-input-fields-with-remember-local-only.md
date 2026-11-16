---
id: f2a5e32d48bc
kind: feature
title: S529.8 sheet input fields with Remember (local-only store)
seq: 3120
status: todo
priority: p1
labels:
    - browser
created: 2026-11-16T17:06:20.396405Z
sprint: 529
sprint_id: 61593fcd-15ea-578f-b8ca-527620119de2
sprint_title: 'bashy browser extension: store distribution + cross-browser (Chrome Web Store, Edge/Windows, Safari)'
---

Operator design 2026-11-16. A sheet field the agent cannot provide shows an input box plus a Remember toggle; the human enters the value in the sheet and it is posted to the hub. Remember on: saved to a LOCAL-ONLY store under a name (operator decision: never synced, never the cloudbox vault), 0600 under BASHY_HOME. Remember off: a one-use 0600 sink like the guard's ask answers. Either way the agent receives only the reference (name or ask id), never the value; later fills use it by reference. Depends on S529.7 (served sheet). Acceptance: tests that the value never reaches agent output/logs, Remember persists across hub restarts, one-use sink is consumed once.
