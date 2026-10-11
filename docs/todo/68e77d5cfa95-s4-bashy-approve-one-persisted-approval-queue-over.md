---
id: 68e77d5cfa95
kind: feature
title: 'S4 bashy approve: one persisted approval queue over ask + @confirm tokens + hitl semantics; ACP and chat grant join'
seq: 1866
status: done
priority: p1
labels:
    - yoke
    - bashsharp
created: 2026-10-11T03:51:49.357572Z
weave: 76
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T13:42:41.255972Z
closed_by: codex-gpt6-sol
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Pieces exist but are unjoined: bashy ask (yoke/pkg/ask: pending request dir, answer socket, single-use latch, ls/answer/claim/cancel, timeout/ttl); @confirm and RFC 0001 (bashy/internal/agentos/confirm.go, bashy/rfcs/0001-agentic-yield.md: argv-derived token = immutable plan hash, yield exit 6 input_required, resume --confirm=TOKEN:yes|no) whose yielded request is NOT persisted; ycode hitl (ycode/internal/harness/stages/hitl: pendingReview, ReportDigest, CAS Version, approve/deny/edit, deny when no human). Build: when @confirm yields (action outside the Bash# @effects envelope) persist an ask-style record kind approval with token, argv, effects, requester, sprint; approve list/show/allow/deny/wait over ask ls/answer/claim; --always maps to chat grant always-allow; the acp ApprovalHandler (replacing BaseHandler auto-allow in yoke/pkg/acp/types.go when S2 permission is not auto) and chat grant create/consume the same record; emit room type approval (S3) and surface pending items in the inbox human lane. Mode review = LLM reviewer through the door, bounded, falls back to ask. Decisions feed Sprint 289 S10 audit (todo e8443312). Acceptance: default auto regression test unchanged; a yielded @confirm becomes a listed approval and approve allow resumes it with the same token; ACP permission request round-trips through the queue; evented.

Conductor verify: `go test ./pkg/ask/... ./pkg/acp/... ./pkg/chat/...` in yoke, plus focused bashy `go test ./internal/agentos/...` on a claimed native host for @confirm integration. Own approval/ask/ACP files; S10 waits for this policy seam. Commit with `Agent:` trailer.

Manager integration 2026-10-11: yoke ask/ACP/chat source from #76 was merged as a prerequisite after manager Mac focused tests passed; manager added exact session/argv redemption guard `76119a7`. This original S4 remains **blocked** on the separate Bash# @confirm producer bridge story `d1438073d581` in bashy, process-level yield/resume tests and final candidate gates. Its yoke merge is a component, not complete S4 acceptance.

Manager acceptance 2026-10-11 13:42 UTC: the Bash# producer and persisted queue are integrated at Bashy `55611ded` and Yoke `7c00f623`; independent Mac `GOWORK=off go test -short -count=1 -timeout 5m ./pkg/ask/... ./pkg/acp/... ./pkg/chat/...` PASS. Exact process binary SHA256 `04335177530635aadf1b90d33a69bbbcf00d356d5989d744d91ad582eee1731e`. Real Bash# `@confirm` yielded exit 6 and one matching persisted session/token/argv/effects/sprint approval; list/show/allow and same-session resume consumed it once. Wrong-session and spent-token replay left the scratch target intact; deny refused it. Room approval events recorded pending/allow/deny/resume (the S3 contiguous sequence gate is separate). Raw sanitized receipt: `/tmp/s418-s4-gate-redacted.json`.

A separately launched private ACP v1 JSON-RPC child made an actual permission request through named-agent `chat.Start`, using the source `ApprovalHandler` and a private room card. The exact Bashy `chat grant --once` CLI resolved the same persisted ask approval; the agent finished `permission:once`. Default grant yielded `permission:always`. Auto mode with the same chat launch completed without a queued approval, preserving the default auto behavior. Sanitized receipts: `/tmp/s418-acp-chat-wrapper-redacted.json`, `/tmp/s418-acp-chat-auto-redacted.json`, `/tmp/s418-acp-chat-live-redacted.json`. Private processes and queues were removed. This accepts S4's shared queue, @confirm, ACP/chat grant and event contract. S10's real Zed/Codync bot integration and S3's contiguous sequence behavior remain separate open outcomes.
