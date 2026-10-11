---
id: 68e77d5cfa95
kind: feature
title: 'S4 bashy approve: one persisted approval queue over ask + @confirm tokens + hitl semantics; ACP and chat grant join'
seq: 1866
status: blocked
priority: p1
labels:
    - yoke
    - bashsharp
created: 2026-10-11T03:51:49.357572Z
weave: 76
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Pieces exist but are unjoined: bashy ask (yoke/pkg/ask: pending request dir, answer socket, single-use latch, ls/answer/claim/cancel, timeout/ttl); @confirm and RFC 0001 (bashy/internal/agentos/confirm.go, bashy/rfcs/0001-agentic-yield.md: argv-derived token = immutable plan hash, yield exit 6 input_required, resume --confirm=TOKEN:yes|no) whose yielded request is NOT persisted; ycode hitl (ycode/internal/harness/stages/hitl: pendingReview, ReportDigest, CAS Version, approve/deny/edit, deny when no human). Build: when @confirm yields (action outside the Bash# @effects envelope) persist an ask-style record kind approval with token, argv, effects, requester, sprint; approve list/show/allow/deny/wait over ask ls/answer/claim; --always maps to chat grant always-allow; the acp ApprovalHandler (replacing BaseHandler auto-allow in yoke/pkg/acp/types.go when S2 permission is not auto) and chat grant create/consume the same record; emit room type approval (S3) and surface pending items in the inbox human lane. Mode review = LLM reviewer through the door, bounded, falls back to ask. Decisions feed Sprint 289 S10 audit (todo e8443312). Acceptance: default auto regression test unchanged; a yielded @confirm becomes a listed approval and approve allow resumes it with the same token; ACP permission request round-trips through the queue; evented.

Conductor verify: `go test ./pkg/ask/... ./pkg/acp/... ./pkg/chat/...` in yoke, plus focused bashy `go test ./internal/agentos/...` on a claimed native host for @confirm integration. Own approval/ask/ACP files; S10 waits for this policy seam. Commit with `Agent:` trailer.

Manager integration 2026-10-11: yoke ask/ACP/chat source from #76 was merged as a prerequisite after manager Mac focused tests passed; manager added exact session/argv redemption guard `76119a7`. This original S4 remains **blocked** on the separate Bash# @confirm producer bridge story `d1438073d581` in bashy, process-level yield/resume tests and final candidate gates. Its yoke merge is a component, not complete S4 acceptance.
