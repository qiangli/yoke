---
id: 0e66bb409cf3
kind: feature
title: 'M1 (yoke) command class axis + src package: class core|protected|skill|external, command-to-source map, pinned fetch into ~/.bashy/src, protected-path check'
seq: 2033
status: done
priority: p0
created: 2026-10-23T16:17:53.051803Z
weave: 614
assignee: muse-conductor
sprint: 458
sprint_id: 3b16e60f-dce3-5dd1-8ed1-4384110d0741
sprint_title: 'bashy is all you need, MVP: invariant core + every other command as editable Bash# skill/script'
closed: 2026-10-23T16:39:57.159079Z
resolution: fixed
closed_by: muse-conductor-p
---

M1 (yoke half). Spec: umbrella docs/bashy-invariant-core.md (Sprint 458 M0, decided 2026-10-23). Repo: yoke ONLY. M1b (e8641fe9) does the bashy mounting afterwards. Do not touch coreutils; derive the class in yoke from the existing origin axis plus the core verb list. Public repo: no hostnames, user ids, cloudbox internals or docs/sprint-* evidence files.

Deliver, as a yoke package with a New*Cmd rod that bashy mounts in M1b:
1. Class axis: Class(cmd) returns core|protected|skill|external. Core = bash builtins, POSIX-required + GNU coreutils names, posix-provider externals, and the 14 verbs agent model tool skill self src git dag commands inspect secret ask claim resource. Bin-managed externals = external. Everything else = skill. A ratchet test fails if any atlas command lacks a class or the core verb set drifts from that list.
2. Command-to-source map: `src show <cmd>` returns repo, Go package path, class, and the running binary's release commit (from build info).
3. `src fetch [repo]`: place an OSS repo at $BASHY_HOME/src/<repo> (default ~/.bashy/src) as a git checkout at the running binary's commit, with local branch `pristine` at that commit. Verify the SHA against build info, use the Sprint 386 offline seed when present, and refuse proprietary repos (allowlist of OSS siblings). A second fetch is a no-op. Use the pure-Go git yoke already uses.
4. Protected manifest: an embedded path list from Appendix A of the record (yoke paths, and bashy/sh paths as repo-qualified strings). `src check` reports any working-tree edit under a core or protected path as a violation (exit 1). Fetch leaves those files read-only.

Acceptance, red/green: unit tests for the class ratchet; show for one core, one protected-component and one skill command; fetch from a local fixture repo into a temp BASHY_HOME (no network); a tampered or mismatched commit refused; a second fetch is a no-op; check flags a protected edit. go test on the touched packages + `go build ./...`. Out of scope: bashy wiring (M1b), rebuild and local PRs (M2), skills (M3b).

== REWORK ROUND 2 (manager review of run 613) ==
Start from the run-613 commits: git cherry-pick 7e2adbb9 (it is preserved at refs/salvage/abandoned-613 in this repo). Do NOT pick 8f4c0523; it only carries the KB.md change. Then apply:
1. ProtectedManifest must transcribe the decision record exactly. Your list protects bashy/internal/agentos, pkg/weave and pkg/steward wholesale. Those are SKILL tier (editable) and must NOT be listed whole. Use these rows (repo, path, kind):
 core (the certified engine, coreutils and the Bash# runtime; a running core never executes a local edit):
  sh: syntax, expand, interp, pattern, fileutil, shell, winmode, pathconv, lower, polyglot
  coreutils: "" (the whole repo)
  readline: "" (the whole repo)
  yoke: pkg/atlas, pkg/src (the class table itself), go.mod, go.sum
 protected (guard rails):
  sh: syntax/bashpp_decorator.go, interp/bashpp_decorator.go (already covered by core; keep them anyway)
  bashy: internal/agentos/decorators.go, decorators_std.go, confirm.go, contain.go, contain_other.go, contain_egress.go, contain_container.go, contain_image.go, limit.go, contracts.go, attest.go, audit.go, inspect_decorators.go, coord.go, verify.go, self.go, self_image.go, self_pair.go, self_seed.go, resource_admission.go  (FILES, not the directory)
  yoke: pkg/policy, pkg/procguard, pkg/secrets, pkg/redact, pkg/ask, pkg/binmgr, pkg/release, pkg/admission, pkg/mplfree, pkg/resources/registry.go, pkg/resources/limits.go, pkg/resources/budget.go, pkg/resources/disk_floor.go, pkg/weave/sprint_commit_attribution.go, pkg/weave/weave_story_commit.go
 Drop your invented rows (yoke git, cmds/all, cmd/yoke, pkg/weave, pkg/steward, bashy internal/agentos as a directory). A path "" means the whole repo; make matchProt handle it.
2. ClassOf(cmd) returns only core | skill | external. Protection is per path, not per command. Remove isProtectedCommand. Add PathClass(repo, path) that returns core | protected | skill, and have `src show <cmd>` list the protected manifest rows in that command's package (if any) as protected_parts. Keep "protected" in the vocabulary for paths.
3. bashBuiltins: add ".", ":" and "[" and remove "time" (time is a reserved word, not a builtin). Assert all 61 in a test.
4. Revert the KB.md change. Weave pre-staged it; it must not be in the diff.

Tests: update the ratchet and manifest tests for the rows above. Add a test that a skill-tier file (e.g. bashy internal/agentos/sprint.go, yoke pkg/weave/weave.go) is PathClass skill and an Appendix-A file is protected. go test ./pkg/src ./pkg/atlas and go build ./... must be green. Then submit.
