---
id: 6fe9a6d49878
kind: bug
title: Weave worker identity and delivery submission accounting
seq: 1970
status: done
priority: p1
created: 2026-10-12T08:01:02.398399Z
assignee: s9999-identity
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T15:10:03.577277Z
closed_by: s9999-maintenance-manager
---

S423: every weave worker commit (yoke runs 152/154/155/156, cloudbox run 3) logged '(bypass) commit: Story #N assignee <worker>, found identity unknown'. Managers must strip BASHY_AGENT on weave start (else workers inherit the manager seat, S329/S422), but weave then sets no identity for the child. weave start should export BASHY_AGENT=<worker instance> to the agent process.

S9999 lane includes separate #1967 d364f12fcc99 automatic story submission despite a following non-delivery KB auto-commit. Fix child worker identity in weave_agent.go using actual instance, not inherited manager. Fix weave_worker_story.go submission to validate worker delivery provenance instead of ignoring entire run when AutoCommitted. Preserve dirty/failed/verify/isolation guards and reject an auto-only provenance-free delivery. Focused red/green regressions for both. Avoid hydration/preservation in weave_impl.go (another lane) and coaching/meet (another lane). Correct own BASHY_AGENT and both required story trailer pairs. No push/canonical changes; no full build/container on Dragon. Send SHA/evidence to s9999-maintenance-manager.

Consolidated retained story: 6fe9a6d49878. Worker delivery accounting delivered together; companion d364f12fcc99. Original UUID and separate acceptance evidence retained in docs/sprint-9999-evidence.md in the umbrella. All original criteria independently verified before closure.
