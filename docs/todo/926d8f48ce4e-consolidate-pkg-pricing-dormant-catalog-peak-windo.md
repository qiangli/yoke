---
id: 926d8f48ce4e
kind: refactor
title: Consolidate pkg/pricing (dormant catalog, peak windows) into pkg/llmprice
seq: 3074
status: todo
priority: p2
created: 2026-11-10T02:56:14.298332Z
sprint: 607
sprint_id: d3d7f687-3414-576c-ad29-b45dfc879257
sprint_title: 'Metering follow-ups: one price table, honest cost and alarms, member go-live guards'
---

Found at S602 close: yoke has two price tables. pkg/llmprice (S602 A2) prices door audit records; pkg/pricing (catalog.yaml, 4 rows incl. deepseek peak windows) has no importers outside its tests. Fold its rows and peak-window rule into llmprice (or delete it), one table.
Sprint 607 F1. Keep pkg/llmprice as the one table (door uses it); move pkg/pricing rows (deepseek) and its peak-window rule in, delete pkg/pricing and its atlas census line. Agent: genie glm-5.3. Tests: unit only in the story; full suites at sprint close (operator rule). Commit trailers one per line.
