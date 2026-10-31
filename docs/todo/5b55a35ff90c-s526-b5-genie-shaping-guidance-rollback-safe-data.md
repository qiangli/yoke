---
id: 5b55a35ff90c
kind: feature
title: 'S526.B5 Genie shaping guidance: rollback-safe data and interface evolution'
seq: 2917
status: todo
priority: p1
labels:
    - shaping
created: 2026-10-29T19:18:21.52267Z
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

Wave 1, B5 (guidance + one example, no new mechanism). Operator 2026-10-29; master plan 'Data evolution'. Put the expand-then-contract rule into the instructions genie's shaping agent receives (yoke/pkg/webconsole shaped candidate/delegate path: find where the shaping request text/brief is built, e.g. shaped_candidate.go / shaped_delegate.go, or the shaping skill text it loads): generated changes ADD properties/elements with a fallback for old data instead of removing or renaming stored data or interface elements (unit names, flags, arguments, routes, declared stored properties); removal only via a separate explicit user-approved contract step. Include one short add-with-fallback example. Acceptance: focused Go test that the shaping brief contains the rule and example; go build ./... green. Live proof (genie candidate adds with fallback) is recorded later in B3 by the manager.
