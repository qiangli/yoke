---
id: 926d8f48ce4e
kind: refactor
title: Consolidate pkg/pricing (dormant catalog, peak windows) into pkg/llmprice
seq: 3074
status: todo
priority: p3
created: 2026-11-10T02:56:14.298332Z
---

Found at S602 close: yoke has two price tables. pkg/llmprice (S602 A2) prices door audit records; pkg/pricing (catalog.yaml, 4 rows incl. deepseek peak windows) has no importers outside its tests. Fold its rows and peak-window rule into llmprice (or delete it), one table.
