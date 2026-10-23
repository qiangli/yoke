---
id: 998bf06de8de
kind: enhancement
title: 'src: add ''src revert <repo>'' (reset local to pristine) and refuse ''src pr open'' when the head has no diff against base'
seq: 2038
status: todo
priority: p3
created: 2026-10-23T18:03:59.637671Z
---

Found in the 2026-10-23 Sprint 458 M2b live smoke. Reverting a host to the release currently needs raw git (checkout local; reset --hard pristine), and pr open accepted PR 1 with head == pristine (an empty diff). Both are agent-UX gaps; the M3 demo uses git for the revert.
