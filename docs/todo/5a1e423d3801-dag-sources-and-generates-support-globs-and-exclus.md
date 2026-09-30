---
id: 5a1e423d3801
kind: bug
title: dag Sources and Generates support globs and exclusions
seq: 6
status: todo
priority: p0
labels:
    - dag
created: 2026-09-30T06:36:19.037774Z
sprint: 335
sprint_id: 43115269-a1b7-58f8-a9a9-07eef6786654
sprint_title: 'bashy dag: reliable incremental builds and reusable task graphs'
---

Finding: pkg/dag/cache.go Fingerprint and UpToDate pass each Sources/Inputs/Generates value to filepath.Join and os.Stat; hashPath handles files or directories, not glob patterns. Go Task taskfile/ast/task.go and website/src/next/docs/reference/schema.md support recursive source/output globs, exclusions and optional gitignore filtering. Acceptance: expand source and output globs deterministically; adding, deleting, or changing a matching file invalidates the target; excluded files do not; an unmatched required generated pattern does not produce a false cache hit; watch mode sees membership changes; regression tests cover all cases and work on Linux, macOS and Windows. Preserve literal path and directory behavior.
