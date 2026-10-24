---
id: eec4b627ecfb
kind: feature
title: 'S498.5 Prune convention: ''bashy <noun> prune [--older] [--apply] [--json]'' (dry-run default), ''bashy prune'' overview with the exact per-noun command, new agent/self/src nouns, weave/sprint/claim brought in'
seq: 2043
status: todo
priority: p1
created: 2026-10-24T03:52:09.111383Z
sprint: 498
sprint_id: 9c13d93e-b866-5ba6-bf54-f3e5b2e47f18
sprint_title: 'Satellite ownership, sharing and prune (launch MVP): two logins, satellite transfer, bashy host/satellite prune'
---

Spec: umbrella docs/satellite-ownership-sharing-prune-mvp.md section 3 (operator 2026-10-23: agents and humans just say 'bashy x prune' to clean stale clutter). Repo: yoke (shared prune contract + noun implementations as rods); the bashy mount (verb wiring, atlas cap 'prune', cobra schema) is a separate bashy run. Deliver: (1) a small shared contract: Item{noun, id, reason, size}, dry-run lists items with reasons, --apply removes and prints counts, --json for agents, never touches in-use or other-owner items. (2) 'bashy prune' (no noun): runs every registered noun's dry run and prints one line per noun with counts/size and the exact command to apply (e.g. 'weave  3 workspaces (1.2 GB)  -> bashy weave prune --apply'); never applies. (3) new nouns: agent prune (cloned/per-sprint seats unused > --older 30d; never base bindings), self prune (old bashy/managed-tool versions and caches, keeping the current and the previous), src prune (~/.bashy/src/.builds older than the current local tip; never branches/PRs). (4) adapt weave prune, sprint prune and claim prune to the same flags and register them in the overview (keep their current semantics). host/satellite nouns come from S498.3. Red/green per noun with temp homes; the overview lists every registered noun; dry-run changes nothing (assert filesystem/db unchanged). Live: run 'bashy prune' on the dev box and report the summary before any --apply.
