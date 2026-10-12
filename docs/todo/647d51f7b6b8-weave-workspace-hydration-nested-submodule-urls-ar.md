---
id: 647d51f7b6b8
kind: task
title: 'weave workspace hydration: nested submodule URLs are not rewritten to local origins because the nested .gitmodules is invisible until git recurse walks it; umbrella-rooted runs then network-clone yoke/external/* and ycode/external/* private forks and fail'
seq: 1905
status: assigned
priority: p1
created: 2026-10-11T10:40:29.488728Z
weave: 186
assignee: s9999-workspace
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

Fix nested-submodule workspace hydration and safe recovery (#1905 with #1911). Original: Sprint 420 umbrella runs 13/14 tried network cloning nested forks because weaveHydrateSubmodules walks only the initially empty workspace. Enumerate SOURCE-root nested .gitmodules and use local-only overrides when populated; canonical URLs must be restored and provisioning timeout bounded. Verify offline nested hydration and existing yoke-rooted behavior.
Related #1911 (530600c9fa32): abandon --force preservation fails for unchanged gitlinks with modified/untracked nested content. Preserve genuine work, including nested work, before deleting; never silently discard actual edits. Prove safe empty interrupted-hydration recovery with a regression fixture; run 109 cleanup needs manager coordination, no worker deletion of unrelated runs.
Scope yoke/pkg/weave hydration/preservation only. Do not alter worker identity, result submission, coaching or feed code. Read CLAUDE.md. Work in submodule checkout, commit with both story trailer pairs if implementing both, never push or mutate canonical checkout. No containers/full builds on Dragon; focused unit tests only. Send SHA, tests and any operational remainder to s9999-maintenance-manager with bashy mb send --as your actual identity. Existing #1911 acceptance retained until independently verified.
