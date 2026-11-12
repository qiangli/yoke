---
id: 540a74e1ddfd
kind: enhancement
title: 'bashy git checkout: detached checkout of a commit SHA, and -q/--quiet'
seq: 3106
status: done
priority: p1
created: 2026-11-12T18:36:06.211282Z
assignee: codex-gpt6-astra
sprint: 619
sprint_id: 92c5ee73-712e-56fc-b41e-a85dfe9d1cb5
sprint_title: 'Relationship spec: groups, signed identity, default-deny outpost, app mapping'
closed: 2026-11-12T18:51:08.891499Z
closed_by: claude-opus5.5-s619
---

Found 2026-11-12 (S619 loom+bashy sandbox gate on the test host): `bashy git checkout <sha>` fails with 'checkout: reference not found' and `checkout -q` with 'unknown shorthand flag: q'. nativeCheckout (git/exec_read.go ~1736) treats a single argument only as a branch name. Gates need a detached checkout of an exact commit. Add: when the single argument is not a branch, resolve it as a commit (resolveCommit) and check out its hash detached, with the same clean-worktree/populate safety as the -b/-B paths and git-like messages; accept -q/--quiet (suppress normal output) on checkout. Tests in git/: detached checkout by full and short SHA, refused on a dirty worktree, -q quiet, branch checkout unchanged.

Correction (conductor, 2026-11-12): the 'reference not found' report was a conductor error - the full SHA passed to the gate did not exist (a mistyped expansion of the 10-char prefix 36262d5427). The CLI path git.Checkout (git.go) already checks out a revision detached. The delivered change (3ced1141) is still valid: it adds detached checkout to the native exec path (nativeCheckout) and -q/--quiet, which was a real gap.
