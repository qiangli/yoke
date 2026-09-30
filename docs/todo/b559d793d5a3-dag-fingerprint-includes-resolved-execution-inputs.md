---
id: b559d793d5a3
kind: bug
title: dag fingerprint includes resolved execution inputs
seq: 7
status: todo
priority: p0
labels:
    - dag
created: 2026-09-30T06:36:19.112404Z
sprint: 335
sprint_id: 43115269-a1b7-58f8-a9a9-07eef6786654
sprint_title: 'bashy dag: reliable incremental builds and reusable task graphs'
---

Finding: pkg/dag/cache.go Fingerprint hashes body, dependency fingerprints and Sources/Inputs only. Target Env, frontmatter vars, CLI KEY=VALUE overrides and other execution-affecting metadata can change while Generates still exists, yielding a false up-to-date result. Acceptance: define and document a stable effective-input fingerprint for output-producing targets; changing relevant resolved variables, target environment or execution mode forces a rebuild; unchanged inputs still skip; avoid writing secret values to cache, logs or JSON; focused regression tests exercise env and CLI overrides. Review which placement/toolchain and include-file changes must invalidate and record the decision.
