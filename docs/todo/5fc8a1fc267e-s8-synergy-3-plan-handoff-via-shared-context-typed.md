---
id: 5fc8a1fc267e
kind: feature
title: 'S8 Synergy 3: plan handoff via shared context - typed tool cmd run results into todo/kb/handoff'
seq: 1870
status: blocked
priority: p2
labels:
    - yoke
created: 2026-10-11T03:51:58.783753Z
weave: 83
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Git stays the cross-host carrier for shared context (Sprint 228): sprint stories, todo and the kb repo ring travel by push; handoff records (yoke/pkg/handoff, ~/.bashy/handoff/ID.json with diff and untracked content) are portable files. bashy tool cmd run --json emits a typed result envelope (tool, command, capability, outcome, artifact text, episode) that pipes into todo add --note -, kb and handoff without glue. Cross-host pickup = commit and push the todo/kb, then mb send coder@hostB with the kb slug and todo uuid (uuid refs resolve on every host; remote mail already rides the cloudbox repo-session task feed). Use a cloudbox task lease only when one cross-host driver is required, gated by Sprint.CanConverge. Recipe dag.md: claude:plan to kb and todo, delegate codex implements citing the kb slug; second variant hands off to an agent on another paired host. Acceptance: dag.md green with two vendors; the plan artifact is visible in kb and todo; the cross-host variant delivers the pointer via mb and the remote agent resumes from it.

Conductor verify: `go test ./pkg/toolcmd/... ./pkg/handoff/... ./pkg/todo/...` in yoke; retain raw two-vendor and paired-host recipe logs before closure. Own typed toolcmd/context files; S9 waits for this file seam. Commit with `Agent:` trailer.

Manager review 2026-10-11 07:44 UTC: source `87757ab` and manager recipe correction `2063622` merged from yoke #83. Independent Mac `GOWORK=off go test ./pkg/toolcmd/... ./pkg/handoff/... ./pkg/todo/...` PASS (`/tmp/s418-mac-s8-87757ab.log`). The manager correction preserved existing todo body and required Sprint/Story/Agent trailers; the hook warned of an assignee mismatch because this was an explicit manager edit on the worker branch. Original S8 remains blocked pending a real two-vendor `dag.md` run, KB/todo artifact verification, MB pointer and paired-host resume transcript. Weave's mechanical done state was reopened; no full acceptance is claimed.

Continuation 2026-10-11 07:36 UTC: GLM run #78 exited -1 after over 30 minutes without a source change. Its preserved WIP `202274a` contains only KB/WEAVE_MEMORY edits and is not delivery code. Resume this original Story-ID in a fresh private yoke tree; keep the exact toolcmd/handoff/todo and paired-host acceptance above. The manager owns the native and two-vendor gates.
