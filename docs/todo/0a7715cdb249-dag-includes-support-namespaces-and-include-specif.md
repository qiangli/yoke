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
sprint: 335
sprint_id: 43115269-a1b7-58f8-a9a9-07eef6786654
sprint_title: 'bashy dag: reliable incremental builds and reusable task graphs'
---

Finding: pkg/dag/parser.go ParseFile merges included targets into one namespace and resolves name collisions by precedence. Go Task supports namespaced includes, include-specific vars and working directory, optional includes and task exclusions (website/src/next/docs/reference/schema.md Include). Acceptance: design a backward-compatible namespace spelling; two included files may both define build and be invoked distinctly; scoped variables and directory resolve consistently for bodies and input paths; missing optional include and excluded targets behave predictably; existing flat include precedence remains compatible; list/JSON and dependency resolution expose qualified names; tests cover local and pinned remote includes.
