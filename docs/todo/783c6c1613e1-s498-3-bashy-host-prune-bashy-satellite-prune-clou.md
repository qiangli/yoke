---
id: 783c6c1613e1
kind: feature
title: S498.3 bashy host prune + bashy satellite prune (cloud side, per the prune convention), plus the CLI for satellite transfer
seq: 2042
status: todo
priority: p1
created: 2026-10-24T03:23:30.01054Z
sprint: 498
sprint_id: 9c13d93e-b866-5ba6-bf54-f3e5b2e47f18
sprint_title: 'Satellite ownership, sharing and prune (launch MVP): two logins, satellite transfer, bashy host/satellite prune'
---

Spec: umbrella docs/satellite-ownership-sharing-prune-mvp.md sections 2-3. yoke (pkg/fleet client + New*Cmd rods), mounted in bashy afterwards as a separate run. host prune: against one server as the owner, list hosts with no heartbeat older than --older (default 14d) plus soft-deleted ones; --apply deletes through the server's host-delete path. The cloudbox side must also delete host assets (HostAssets.DeleteByHost is never called today) and dangling HostShares; the server half of that is a small cloudbox change in this story's sibling run. satellite prune: against the master as the owner, never-enrolled provisioning entries and lost satellites via the existing lost/destroy paths. Both print exactly what would be removed and need --apply. Transfer CLI: satellite transfer <name> --to <email> | accept <id> | cancel <id> | list. Red/green with a fake server; live dry-run on the master and one satellite before any --apply.
