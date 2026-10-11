---
id: d4d28b59d4a3
kind: feature
title: 'S6 Synergy 2: messenger as input - CLI front door on the meet DM engine with app-pair style sender pairing'
seq: 1868
status: todo
priority: p1
labels:
    - yoke
    - bashy
created: 2026-10-11T03:51:54.078581Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Do NOT revive the retired bashy relay verb (docs/bashy-relay-design.md retired it; meet DMs still call themselves Relay DMs). The inbound engine exists: meet DM path POST /api/dms/AGENT/messages and /work into chat.Invoke with a stored transcript (yoke/pkg/meet/relay_dm.go, serve.go). Pairing exists: bashy app pair single-use scoped expiring tickets with devices list/revoke (yoke/pkg/webconsole pairing). Add a CLI front door on the same handler: bashy meet dm AGENT --channel C --sender S [--thread T] reading TEXT from argv or JSON on stdin and printing the reply on stdout, callable from any gateway hook (OpenClaw hook or skill, Codync routine webhook, ycode chat hub, bot script). Routes are Bash# functions in skills/agent-hub.md: @agent text, /approve ID and /deny ID (to S4), /status, /meet ROOM text; else the sender bound agent (S2 bindings). Unknown senders get a code minted by the app-pair ticket machinery; the allowlist has the app-device shape with scopes ask|control|approve, expiry, revoke. Gateway bot tokens live in bashy secret; the messenger account is a Sprint 408 resource claim so one host runs the bot. Acceptance: unpaired sender gets a code and no agent runs; after approval the same message reaches the agent and the reply returns on stdout; /approve resolves an S4 approval; every message is an S3 event.

Sprint 418 pointed execution: implement yoke meet CLI/front door, reuse the existing webconsole pairing store and S3 event stream, and route approvals through the shared S4 queue. Coordinate with the active Windows chat lane by keeping edits in meet/webconsole/commscli where possible; do not edit chat socket/path tests or S18 outpost source. Worker verify: unpaired sender cannot invoke, pairing ticket single-use with scope/expiry/revoke, paired sender gets stdout reply, /approve resolves shared queue, each message appears in the timeline; run GOWORK=off go test ./pkg/meet/... ./pkg/webconsole/... ./test/commscli/... . Manager owns final two-host and bot-hook native gates. Dragon short tests only, remote native gates through claimed host. Commit Sprint/Story/Story-ID/Agent trailers.
