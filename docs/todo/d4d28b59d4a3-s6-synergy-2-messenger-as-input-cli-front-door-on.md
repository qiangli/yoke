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

Manager continuation 2026-10-11 11:48 UTC: source and focused tests are already integrated; old weave #96 is terminal and its stale metadata was cleared. Resume this original story for a bounded **real process** gate, not more framework code: under an isolated home, show unknown sender receives one pair code and does not launch an agent; approve that code with ask/approve scope, send the original DM to a pinned available agent and capture stdout reply plus timeline event; issue a real S4 approval and resolve it with `/approve ID`, recording the same approval ID and an event. Revoke/expiry must refuse later delivery. Use current exact candidate Yoke `f132e07` and Bashy `244740fe` where practical. Verify source with `GOWORK=off go test -short -timeout 5m ./pkg/meet/... ./pkg/webconsole/... ./test/commscli/...`; native/live gate may use a claimed host, but do not disturb the paired DO/Mac services currently under S18. Keep channel bot-account gateway proof separate if no claimed messenger account/token exists. Preserve raw receipt categories without exposing pair codes or tokens. Submit partial evidence honestly; manager will keep S6 open until all original acceptance is met.

Manager verdict 2026-10-11 11:55 UTC: Sonnet #115 commit `c4f2e87` merged as `9410642` after direct script review and independent execution of `GOWORK=off scripts/meet-dm-gate.sh` (all checks passed). The isolated commscli audit binary and deterministic stub prove unknown/pending sender, scoped pair approval, one-use code, stdout/JSON delivery, same-ID S4 `/approve`, timeline, revoke and expiry. This does not prove the Bashy host mount, a real model response through a channel gateway, or a claimed messenger bot account. The original story remains open; source and failure history are preserved. Future work should use this existing script and story, not add another wrapper or child story.
