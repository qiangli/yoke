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
---

Finding: pkg/dag/parser.go ParseFile merges included targets into one namespace and resolves name collisions by precedence. Go Task supports namespaced includes, include-specific vars and working directory, optional includes and task exclusions (website/src/next/docs/reference/schema.md Include). Acceptance: design a backward-compatible namespace spelling; two included files may both define build and be invoked distinctly; scoped variables and directory resolve consistently for bodies and input paths; missing optional include and excluded targets behave predictably; existing flat include precedence remains compatible; list/JSON and dependency resolution expose qualified names; tests cover local and pinned remote includes.

Rescope 2026-10-10 (operator, MVP per KISS / rod not fish): DEFERRED out of Sprint 402 and unlinked. This copies a Go Task feature; plain includes plus local override, Matrix, and Require/Ensure/Generates cover current needs, and native build tools keep their own incrementality. Reopen only when a real project hits the limit. Plan of record: Sprint 403 spec docs/sprint-367-master-execution-plan.md (rescope section).
