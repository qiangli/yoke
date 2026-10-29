---
id: 7d177fb33a48
kind: feature
title: 'S515.5b yoke half of S515.5: bashy loom mirror add|list|rm — push mirror to a non-Gitea remote (GitHub) reusing syncMirror/listMirrors/localNetworkHint'
seq: 2815
status: todo
priority: p1
labels:
    - loom
created: 2026-10-28T06:33:41.131975Z
assignee: claude-opus5
sprint: 515
sprint_id: e82160ea-95db-5f2a-b69e-b2d784b2996b
sprint_title: 'satellite loom: private forge + CI + groups (KISS/MVP, reuse loom/act_runner/scopes)'
---

Companion of S515.5 (umbrella 3151ba39; evidence + cutover runbook umbrella docs/s515-5-cloudbox-on-loom.md on origin/dev c0fabf0e8). Found live: `bashy loom replica` is Gitea-to-Gitea by construction (ensureReplicaOwner/ensureReplicaRepo provision the target), so it cannot target GitHub; the S515.5 run used a raw POST /push_mirrors. Rule: agents use bashy commands only — close the gap.
Do (external/loom, KISS, reuse replica.go helpers):
- `bashy loom mirror add --repo O/N --remote URL --username U --token-file F [--sync-on-commit] [--interval D]`: create a Gitea push mirror to an existing remote (no target provisioning); token read from a file (0600) or stdin, never argv/env echo; immediate sync like replica add; the 401/local-network hint (LOOM_MIRROR_ALLOWED_DOMAINS incl. github.com) as in replica.
- `bashy loom mirror list [--repo O/N]` and `rm --repo O/N --remote URL`.
- Help text states the force-sync/prune hazard (a push mirror deletes target refs missing on the source) and that a manual commit on the target is overwritten.
Unit tests with a fake Gitea API (add/list/rm, token never in argv/logs, idempotent add). Gate: go test ./external/loom; GOOS=windows go vet ./external/loom. No live run needed (the mechanism was proven live in S515.5); do NOT create GitHub repos. No host names in this repo. Bounded 45 min.
Commit trailers: Sprint: #515 / Story: #2815 / Story-ID: 7d177fb33a48.
