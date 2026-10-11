---
id: ec8c6600f5bc
kind: enhancement
title: dag supports explicit up-to-date status checks
seq: 10
status: todo
priority: p1
labels:
    - dag
created: 2026-09-30T06:36:19.335488Z
---

Finding: dag's up-to-date decision is limited to Generates existence plus fingerprint equality (pkg/dag/cache.go). Go Task status commands can validate external or semantic output state. Acceptance: add a documented, side-effect-free Status check that must pass before skipping an output-producing target; false or errored check forces execution or fails with a clear diagnostic according to a documented rule; --explain reports the decision; tests cover stale external state with unchanged files, successful skip and failed check. Keep Ensure as a postcondition and Require as a precondition.

Rescope 2026-10-10 (operator, MVP per KISS / rod not fish): DEFERRED out of Sprint 402 and unlinked. This copies a Go Task feature; plain includes plus local override, Matrix, and Require/Ensure/Generates cover current needs, and native build tools keep their own incrementality. Reopen only when a real project hits the limit. Plan of record: Sprint 403 spec docs/sprint-367-master-execution-plan.md (rescope section).
