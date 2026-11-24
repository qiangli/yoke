---
id: 9b40514b3aa5
kind: bug
title: 'fleet: zcode-glm-5.3 probe unusable — the tool rejected a flag (unknown flag), launch contract drifted; bare opencode probe lacks SMOKE-OK and blocks opencode-glm-5.3 at assign'
seq: 3153
status: todo
priority: p2
created: 2026-11-24T14:32:08.405149Z
---

2026-11-24 06:27-06:30 PST, dev box, Sprint 691 H6b staffing: weave fleet --fleet zcode-glm-5.3 --probe -> UNUSABLE 'the tool rejected a flag (unknown flag) — its launch contract has drifted'. opencode-glm-5.3 binding probe USABLE, but sprint assign refused it ('not an eligible fleet binding') because the tool-level opencode probe is cached UNUSABLE (smoke output lacked SMOKE-OK, default model). Two defects: zcode launch flags, and the assign gate letting a tool-level probe override a fresh exact-binding probe. Workaround used: ycode-glm-5.3.
