---
id: 02d36388ceba
kind: task
title: 'S515.5c yoke half of S515.5: one-way never-delete backup interface for loom repos (adapters: local bare repo + github)'
seq: 2934
status: done
priority: p0
created: 2026-10-29T20:34:16.52211Z
weave: 723
assignee: codex-gpt6-astra
sprint: 515
sprint_id: e82160ea-95db-5f2a-b69e-b2d784b2996b
sprint_title: 'satellite loom: private forge + CI + groups (KISS/MVP, reuse loom/act_runner/scopes)'
closed: 2026-10-29T20:58:35.707324Z
resolution: fixed
closed_by: codex-gpt6-astra-u
---

Companion of dhnt story 3151ba39 (read it: umbrella docs/todo/3151ba39261f-*.md). OPERATOR RULES 10-29: backup target = one pluggable backend interface/adapter with a NEVER-DELETE contract: push new commits/refs only (fast-forward or new refs), no prune, no force-push; a ref that diverged on the target is NOT overwritten - the sync fails loudly and reports it. Gitea's built-in push mirror force-syncs and prunes: do NOT use it as-is for backups. Name it 'backup' in verbs/docs/UI (e.g. bashy loom backup add|list|rm|sync; fix 'bashy loom mirror' if it relies on pruning for GitHub targets). Adapters THIS sprint: 'local' (bashy-managed bare git repo at any path; works on a network drive / p2p folder; the hermetic test target) and 'github'. No S3/other-git/cloud-drive adapters now; document the interface for future adapters. Acceptance: local adapter proven by hermetic red/green tests (new refs pushed, ff pushed, diverged ref -> loud failure + target unchanged, deleted source branch -> target branch kept); github adapter live: commit pushed to the peer loom's cloudbox repo appears on a throwaway PRIVATE GitHub repo with no manual step (both SHAs + timing), survives a loom restart. NEVER point it at the real GitHub cloudbox repo (operator go required). Venue: S515.6 peer loom (bashy claim list | grep s515-ci; cloudbox docs/s515-6-ci-on-peers.md on origin/dev). Heavy builds on elephant, not Dragon.
