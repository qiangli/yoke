---
id: 0a7715cdb249
kind: enhancement
title: dag includes support namespaces and include-specific configuration
seq: 8
status: todo
priority: p1
labels:
    - dag
created: 2026-09-30T06:36:19.185427Z
sprint: 402
sprint_id: 107fb1ff-6f9c-5bc1-8896-a26f792a6fd8
sprint_title: 'bashy dag post-1.0: reusable task graphs and remote execution'
---

Finding: pkg/dag/parser.go ParseFile merges included targets into one namespace and resolves name collisions by precedence. Go Task supports namespaced includes, include-specific vars and working directory, optional includes and task exclusions (website/src/next/docs/reference/schema.md Include). Acceptance: design a backward-compatible namespace spelling; two included files may both define build and be invoked distinctly; scoped variables and directory resolve consistently for bodies and input paths; missing optional include and excluded targets behave predictably; existing flat include precedence remains compatible; list/JSON and dependency resolution expose qualified names; tests cover local and pinned remote includes.
