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
sprint: 402
sprint_id: 107fb1ff-6f9c-5bc1-8896-a26f792a6fd8
sprint_title: 'bashy dag post-1.0: reusable task graphs and remote execution'
---

Finding: pkg/dag/graph.go BuildGraph resolves Requires as target names only. Go Task supports dependency and task calls with variables and loops. Acceptance: a parent can invoke the same reusable target with distinct parameter sets without copying target definitions; each call has an unambiguous graph identity, correct dependency ordering, environment and cache key; list/plan/JSON show resolved invocations; duplicate identical calls have defined deduplication behavior; cycle detection and tests cover nested calls. Keep Matrix behavior compatible.

Review 2026-10-08 (moved from Sprint 335 to post-1.0 Sprint 402): still absent. Related bug to fix here: splitList (pkg/dag/parser.go) splits Requires on commas, so a multi-key matrix child such as build:arch=amd64,os=linux cannot be named in Requires (single-key children only). Parameterized clones need the resolved-env fingerprint from b559d793d5a3 (Sprint 379) or they share a cache key.
