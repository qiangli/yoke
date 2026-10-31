---
id: 5296f987c36f
kind: refactor
title: S526.B1a Shape the bashy app web UI presentation (embedded SPA -> shaped kind)
seq: 2875
status: todo
priority: p1
labels:
    - software-shaping
created: 2026-10-28T23:28:40.819079Z
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

Planning authorized 2026-10-28; master plan approved 2026-10-28; bashy app web UI selected as app #1 2026-10-29. Spec: docs/sprint-526-software-shaping-plan.md; master plan: docs/software-shaping-epic.md. Dependencies: Sprint 525 accepted; B0 (4eaa396026ef).

Migrate the `bashy app` web UI first, then the other B0-selected apps. The operator is its daily user: the operator's own UI wishes, made through Genie, are the primary real journeys and are tested daily; record each wish, the resulting version and its outcome. They use shared units/contracts, Genie preview, per-user revisions and version history, mandatory gates, Weave isolation, Git/Loom history and same-host promotion. No private bypass or app-only privilege. Remaining eligible ledger entries stay as later work. Acceptance: each selected app maps to accepted migration evidence; any missing capability became a generic public extension.
