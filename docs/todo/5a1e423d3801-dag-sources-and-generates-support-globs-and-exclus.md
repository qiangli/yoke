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
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

Finding: pkg/dag/cache.go Fingerprint and UpToDate pass each Sources/Inputs/Generates value to filepath.Join and os.Stat; hashPath handles files or directories, not glob patterns. Go Task taskfile/ast/task.go and website/src/next/docs/reference/schema.md support recursive source/output globs, exclusions and optional gitignore filtering. Acceptance: expand source and output globs deterministically; adding, deleting, or changing a matching file invalidates the target; excluded files do not; an unmatched required generated pattern does not produce a false cache hit; watch mode sees membership changes; regression tests cover all cases and work on Linux, macOS and Windows. Preserve literal path and directory behavior.

Review 2026-10-08 (moved from Sprint 335 to Sprint 379 for v1.0): still absent on main. cache.go Fingerprint passes each Sources/Inputs value to hashPath (os.Stat), so a glob such as src/*.go hashes to the constant "absent" - editing a matching file never invalidates a target whose Generates exists (silent false up-to-date). A glob Generates pattern never matches os.Stat, so the target always reruns. watch.go watchFingerprints has the same flaw. dag.md promises a content-hashed up-to-date skip, so this is a v1.0 correctness bug. Expected change about 150 LOC: one matcher (** and !exclusions) shared by Fingerprint, UpToDate and watch; existing fingerprints change once.
