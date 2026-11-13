---
id: d769d6a52179
kind: bug
title: sprint assign cannot dispatch a named agent clone; busy guard frees an identity on submit
seq: 3109
status: todo
priority: p1
labels:
    - weave
created: 2026-11-13T00:11:53.236499Z
---

Found by Sprint 627 conductor 2026-11-13 (operator mb:13525 identity collision). Root cause in pkg/weave/sprint_assign.go:
1. --agent canonicalization (~L462-473) maps the name to its tool:model MatrixKey and substitutes the FIRST pool entry with that key, so after 'bashy agent clone claude-opus5.5 claude-opus5.5-m1', 'sprint assign --agent claude-opus5.5-m1' dispatches as the parent claude-opus5.5 (runs umbrella#13/#14 claimed M1 419f7b07 as claude-opus5.5 while it held S626 S1 a938e7d9). With --exclude-agent claude-opus5.5 the clone is rejected: 'not an eligible fleet binding' - registered local clones are not in sprintAssignPool.
2. Busy guard: an agent whose run is 'submitted' (story still claimed, awaiting manager) counts as Free, so a second story goes to the same identity.
Fix: exact-name match before MatrixKey fallback; include registered clones in the pool; treat an identity holding a claimed-but-unaccepted story as busy. Verify: pkg/weave unit tests for clone dispatch and submitted-busy.
