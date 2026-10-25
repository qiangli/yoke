---
id: 3e4410c0a71a
kind: bug
title: weave start --resume mints '<agent>-o' and sprint claim refuses it (owns nothing here), so a rework cannot resume its own run
seq: 2035
status: todo
priority: p2
created: 2026-10-23T17:35:00.88211Z
---

Found 2026-10-23 Sprint 458: 'bashy weave start --resume --run 613 -- muse-conductor <body>' (and with muse-spark1.3) failed: 'claim worker story: sprint manager "muse-conductor-o" owns nothing here'. The story assignee was muse-conductor. The workaround was to abandon, append the rework to the story and re-assign, which loses the worker context. Expected: resume reuses the run's worker identity (or its clone) and claims the story it already holds.
