---
id: 8b485840e955
kind: bug
title: loom backup/proxy gaps found in the S515.5 cutover round
seq: 2941
status: todo
priority: p1
created: 2026-10-30T12:08:16.459318Z
---

From S515.5 (#206, umbrella docs/s515-5-cloudbox-on-loom.md, 2026-10-30). (1) loom REST API returns 401 through the product proxy (ENABLE_REVERSE_PROXY_AUTHENTICATION_API unset): web UI signed in, API not; an org cannot create loom repos via the product path (round used the gitea admin CLI). Blocks 'configuration only'. (2) clone URL shown to members is https://127.0.0.1:31880/...: gitea builds self-URLs from forwarded scheme + rewritten Host; rewriteUpstreamOrigin only matches http://. (3) 'loom backup sync' prints cobra usage on runtime errors (no SilenceUsage), burying the divergence message. (4) 'loom mirror list' fails on a primary without loom-state.json (the runbook's pre-migration check). (5) no backup status surface: success silent, failures only in logs (a 13-min divergence went unnoticed). (6) branches absent on loom (e.g. GitHub main / open PRs) can never raise divergence: silent gap. (7) pre-flip steady state needs a scheduled forge->loom fetch per branch. Fix with tests; no rc candidate edits while S525 verification runs.
