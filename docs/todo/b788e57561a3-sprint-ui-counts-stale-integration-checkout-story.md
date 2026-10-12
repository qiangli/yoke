---
id: b788e57561a3
kind: bug
title: Sprint UI counts stale integration-checkout story copies as open
seq: 1996
status: assigned
priority: p1
created: 2026-10-12T16:26:02.078398Z
assignee: s9999-maintenance-manager
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

User reports Sprint 9999 UI shows 11 open while sprint show reports 1. Reproduced steward board: 10 stale open copies from temporary s9999-maintenance-integration-yoke plus canonical admin story; canonical registered stores have 36/37 closed. Fix board projection to resolve sprint-linked records through authoritative registered story roots and avoid duplicate checkout snapshots, retaining independent unlinked tasks and explicit read failures. Acceptance: regression fixture with stale checkout and done registered source yields one story, correct count and canonical detail; relevant board tests and Sprint browser test pass; installed app/UI reflects current sprint count. Scope only this user-requested discrepancy; no additional fixes/stories.
