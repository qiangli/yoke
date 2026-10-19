---
id: 5bde08ecfcce
kind: chore
title: Clean redundant local branches in bashy and yoke
seq: 2022
status: done
created: 2026-10-13T17:42:41.471861Z
assignee: s442-manager
sprint: 442
sprint_id: 47120153-446b-558a-8499-edbe32cb6b0a
sprint_title: Clean redundant bashy and yoke local branches
closed: 2026-10-14T07:48:00.077442Z
closed_by: s442-manager
---

User-requested local branch cleanup across bashy and yoke.

Completed 2026-10-13: removed 15 redundant local branches and two clean historical maintenance integration worktrees. Every deleted branch was either an ancestor of main or had all patches equivalent to main according to git cherry. Recovery bundles verified before deletion. Main commit IDs unchanged; remote refs untouched; unique commits and all Sprint 436 linked worktrees preserved. No code tests required for Git metadata cleanup.

Recovery bundles, original branch SHAs, before-state and exact result: $HOME/.bashy/backups/branch-cleanup-20261013-104255

bashy deleted: s436/audit-gate-98, s436/compiler-fix-100, s436/compiler-inputs-99, s436/native-gate-97, s9999-e2e-pin, s9999-inbox-cap-candidate, s9999-inbox-host-candidate, s9999-maintenance-integration.
Preserved: s434/headlamp-8a3ca0e3, s434/headlamp-b2976d73, s436/prima-admission, s436/prima-admission-staging, s436/runner-release-draft.

yoke deleted: s9999-atlas-candidate, s9999-bus-candidate, s9999-fixture-candidate, s9999-genie-candidate, s9999-identity-candidate, s9999-maintenance-integration, s9999-workspace-rework-candidate.
Preserved: s436/ollama-managed-guard, s436/sphere-defaults, s9999-dag-candidate, s9999-fmt-candidate, s9999-genie-rework-candidate, s9999-stream-rework-candidate.

Tracking correction: initially linked to historical 9999 before reading its closure spec; moved to dedicated Sprint 442 to preserve historical closure. Operational work is complete; formal sprint acceptance remains pending with no manager assigned. No delivery commit made.
