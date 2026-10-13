---
id: cbe342530454
kind: bug
title: Warn on sprint show when conductor inbox backlog nears admission limit
seq: 2023
status: todo
priority: p2
created: 2026-10-13T17:57:43.836616Z
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
---

Follow-up from accepted 838a42c825d3: overflow recovery and fresh-clone cursor are verified, but sprint show does not display the requested warning when the conductor seat unread backlog nears 1024 bytes. Add a bounded warning based on the seat mailbox size/count with a focused presentation regression; preserve fail-closed admission and existing fresh-clone behavior.
