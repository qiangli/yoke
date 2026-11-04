---
id: 0bb55463319c
kind: feature
title: C3b Browser hub as an opt-in outpost service (start from outpost admin)
seq: 3046
status: todo
priority: p1
labels:
    - outpost
created: 2026-11-04T18:08:03.968402Z
sprint: 563
sprint_id: 282b20a5-f15c-55eb-a314-eef9580e19d1
sprint_title: 'Product UAT: Klatch invite/chat journeys from a clean install, with defect closure'
---

Add a baseline service unit browser-hub that runs 'bashy browser hub' (live-mode WebSocket hub for the MV3 extension, loopback 58082) under the outpost/bashy supervisor, disabled by default, so the owner or any agent can start it from the outpost admin service units (outpost builtins set --service browser-hub=on) instead of a foreground terminal. Loopback only; no new code paths beyond the unit and its golden default. Acceptance: unit embedded, golden default recorded (opt-in), yoke fleet tests green, unit starts and stops via outpost builtins on a host. Shipping waits for the S561 freeze on bashy pins. Sprint 563 plan: docs/plan-s563-master-execution.md.
