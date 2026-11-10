---
id: c92622f172a9
kind: bug
title: 'A2 fix: pkg/llmprice has no atlas census entry'
seq: 3073
status: done
priority: p1
created: 2026-11-10T02:56:14.187417Z
weave: 26
assignee: agy-gemini3.8-flash
sprint: 602
sprint_id: 945d80da-6344-5fa1-8afb-08d6cac5d7a0
sprint_title: 'Metered shared LLM access: per-peer usage, cost and limits on the LLM door (provider-agnostic)'
closed: 2026-11-10T03:06:39.337515Z
closed_by: claude-opus5.5-s602
---

Sprint close full suite: pkg/atlas TestCensusCoversEveryPackage reports 'pkg/llmprice has no atlas census entry' (new with A2). The test already fails on the pre-sprint baseline for pkg/nativepath - that pre-existing failure is NOT in scope; add only the llmprice entry in pkg/atlas/packages.go next to "pricing" (supPkg/libPkg form, one line: per-model token price table the LLM door uses for cost_usd/unpriced in its audit records). Verify: GOWORK=off go test ./pkg/atlas -run TestCensusCoversEveryPackage now reports only pkg/nativepath. Unit only. Commit trailers one per line.
