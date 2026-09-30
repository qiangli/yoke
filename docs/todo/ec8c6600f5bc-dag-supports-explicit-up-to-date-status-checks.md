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
sprint: 335
sprint_id: 43115269-a1b7-58f8-a9a9-07eef6786654
sprint_title: 'bashy dag: reliable incremental builds and reusable task graphs'
---

Finding: dag's up-to-date decision is limited to Generates existence plus fingerprint equality (pkg/dag/cache.go). Go Task status commands can validate external or semantic output state. Acceptance: add a documented, side-effect-free Status check that must pass before skipping an output-producing target; false or errored check forces execution or fails with a clear diagnostic according to a documented rule; --explain reports the decision; tests cover stale external state with unchanged files, successful skip and failed check. Keep Ensure as a postcondition and Require as a precondition.
