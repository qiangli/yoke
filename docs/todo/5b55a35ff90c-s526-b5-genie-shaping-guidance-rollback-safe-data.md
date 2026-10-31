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

Operator 2026-10-29. Master plan section 'Data evolution: every change stays rollback-safe'. Put the rule into genie's shaping instructions/skill so the changes it generates follow expand-then-contract: never remove or rename a stored property/file/key or an interface element (flag, argument, unit name, route); add new ones; new code reads new first and falls back to old; a changed meaning or type is a new property; migrations are additive, lazy and reversible; contracting is a separate explicit user decision. Minimal: guidance text plus one example in the shaping skill, no new mechanism. Acceptance: on a bashy app UI wish that changes stored data, genie's candidate adds a property with fallback instead of renaming it, recorded in the B3 journeys.
