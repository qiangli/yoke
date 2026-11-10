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

Found at S602 close: yoke has two price tables. pkg/llmprice (S602 A2) prices door audit records; pkg/pricing (catalog.go + catalog.yaml, 4 rows incl. deepseek peak windows) has no importer anywhere in yoke, only its own tests.

Sprint 607 F1, re-scoped 2026-11-10 (plan: dhnt-docs docs/sprint-607-master-execution-plan.md): DELETE, do not merge. Remove pkg/pricing (catalog.go, catalog.yaml, catalog_test.go) and its pkg/atlas census row (which wrongly says pkg/llmbudget consumes it). pkg/llmprice already prices deepseek-chat, deepseek-v4-flash and deepseek-v4-pro; its numbers stay exactly as they are — a number changes only in a commit that cites the provider's public price page (the catalog's numbers differ and carry no provenance). The catalog's peak windows have no source and llmprice.Cost takes no instant: not carried over; a sourced peak rule would be a new story. Gate: go test ./pkg/llmprice/ ./pkg/atlas/ ./pkg/broker/... Agent: genie glm-5.3. Work in a weave workspace, never the shared checkout. Tests: unit only in the story; full suites at sprint close (operator rule). Commit trailers one per line.
