---
id: 783c6c1613e1
kind: feature
title: S498.3 bashy host prune + bashy satellite prune (cloud side, per the prune convention), plus the CLI for satellite transfer
seq: 2042
status: done
priority: p1
created: 2026-10-24T03:23:30.01054Z
assignee: codex-gpt6-sol
sprint: 498
sprint_id: 9c13d93e-b866-5ba6-bf54-f3e5b2e47f18
sprint_title: 'Satellite ownership, sharing and prune (launch MVP): two logins, satellite transfer, bashy host/satellite prune'
closed: 2026-10-25T10:12:06.832453Z
closed_by: s498-manager
---

Spec: umbrella docs/satellite-ownership-sharing-prune-mvp.md sections 2-3. yoke (pkg/fleet client + New*Cmd rods), mounted in bashy afterwards as a separate run. host prune: against one server as the owner, list hosts with no heartbeat older than --older (default 14d) plus soft-deleted ones; --apply deletes through the server's host-delete path. The cloudbox side must also delete host assets (HostAssets.DeleteByHost is never called today) and dangling HostShares; the server half of that is a small cloudbox change in this story's sibling run. satellite prune: against the master as the owner, never-enrolled provisioning entries and lost satellites via the existing lost/destroy paths. Both print exactly what would be removed and need --apply. Transfer CLI: satellite transfer <name> --to <email> | accept <id> | cancel <id> | list. Red/green with a fake server; live dry-run on the master and one satellite before any --apply.

## Worker brief (s498-manager, 2026-10-24). One repo: yoke. Base: yoke main 7661029f (has pkg/prune from S498.5).
SUPERSEDES the "accept/cancel/list" wording above: transfer is ONE step with mutual Members (no pending/accept).
1. host prune noun: registers into pkg/prune. DryRun = `GET /api/v1/hosts?stale=<--older, default 336h>` against one server (as the owner, Bearer from the existing bashy cloudbox token plumbing); Apply = existing owner `DELETE` per host. Reasons come from the server (stale/never_seen/deleted).
2. satellite prune noun: DryRun = master `GET /api/v1/satellites?prune=1`; Apply = existing lost/destroy paths per entry.
3. `satellite transfer <name|id> --to <email> [--yes]`: resolves the satellite, asks "Transfer <name> and everything it owns to <email>?" unless --yes, then `POST /api/v1/satellites/:id/transfer {"to":...}` (satellites:write). Print the server's from/to/transferred_at/note; on 409 print the server's reason verbatim; 404 = "not found or not yours".
Red/green with an httptest fake server (no real network, temp BASHY_HOME). No build outputs in commits. Gate: go test touched packages + go build -o /dev/null ./... . Commit with this story's trailers.

## Rework 1 (s498-manager, 2026-10-24). Run #621: code fine, but yoke is PUBLIC: test fixtures use the real satellite name "ivy" (pkg/prune/rods/cloudbox_test.go, e.g. {"id":"sat-1","name":"ivy"} and SetArgs "ivy"). Start from refs/salvage/abandoned-621 (commit a9bb34a7), cherry-pick onto current yoke main (d4fed99c), replace every real fleet name with a neutral one ("sat-a", "example-sat"). `git diff origin/main | grep -niE 'ivy|alien|zoo|lern|elephant|novi|dragon'` must be empty. No other changes. Gate: go test ./pkg/fleet/ ./pkg/prune/... + go build -o /dev/null ./pkg/... . One commit with this story's trailers.
