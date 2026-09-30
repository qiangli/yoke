---
id: 853c292b4ab2
kind: enhancement
title: dag dependencies support parameterized target calls
seq: 9
status: todo
priority: p1
labels:
    - dag
created: 2026-09-30T06:36:19.258144Z
sprint: 335
sprint_id: 43115269-a1b7-58f8-a9a9-07eef6786654
sprint_title: 'bashy dag: reliable incremental builds and reusable task graphs'
---

Finding: pkg/dag/graph.go BuildGraph resolves Requires as target names only. Go Task supports dependency and task calls with variables and loops. Acceptance: a parent can invoke the same reusable target with distinct parameter sets without copying target definitions; each call has an unambiguous graph identity, correct dependency ordering, environment and cache key; list/plan/JSON show resolved invocations; duplicate identical calls have defined deduplication behavior; cycle detection and tests cover nested calls. Keep Matrix behavior compatible.
