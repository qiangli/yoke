---
id: b559d793d5a3
kind: bug
title: dag fingerprint includes resolved execution inputs
seq: 7
status: done
priority: p0
labels:
    - dag
created: 2026-09-30T06:36:19.112404Z
assignee: muse-spark1.3
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-09T09:05:33.688861Z
closed_by: claude-opus5.5
---

Finding: pkg/dag/cache.go Fingerprint hashes body, dependency fingerprints and Sources/Inputs only. Target Env, frontmatter vars, CLI KEY=VALUE overrides and other execution-affecting metadata can change while Generates still exists, yielding a false up-to-date result. Acceptance: define and document a stable effective-input fingerprint for output-producing targets; changing relevant resolved variables, target environment or execution mode forces a rebuild; unchanged inputs still skip; avoid writing secret values to cache, logs or JSON; focused regression tests exercise env and CLI overrides. Review which placement/toolchain and include-file changes must invalidate and record the decision.

Review 2026-10-08 (moved from Sprint 335 to Sprint 379 for v1.0): still absent on main. Fingerprint hashes body, dependency fingerprints and Sources/Inputs only; Task.Env, frontmatter vars, CLI KEY=VALUE, Host and Lang are not hashed (expandVars substitutes metadata only; the body reads values from env at run time). A var used only in the body changes nothing in the fingerprint - silent false up-to-date. Matrix children differ only in Env and avoid collision only because the cache is keyed by target name. Expected change about 30 LOC (hash sorted resolved env, values hashed not stored, plus Host and Lang). Also add a "what invalidates the cache" section to bashy/docs/dag.md.
