---
id: c8283ebb3f9d
kind: feature
title: 'S5 Synergy 1: agent as LLM service - bashy llm env --client openclaw|codync|generic'
seq: 1867
status: done
priority: p1
labels:
    - yoke
    - bashy
created: 2026-10-11T03:51:51.724068Z
weave: 80
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T12:43:09.243246Z
closed_by: codex-gpt6-sol
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Any agentic tool that takes an OpenAI or Anthropic base URL uses a fleet agent served by the bashy llm door (cligw, port 24556) as its brain. Add bashy llm env --client NAME printing the ready provider block for that client (OpenClaw provider config, generic OpenAI env); a model add example registers an external OpenAI-compatible gateway as a model. Sphere reach is configuration only: the door is shared across hosts via outpost mesh service add llm / mesh dial (Sprint 308 MVP); llm env --client gains --via mesh printing the dialed local address. Acceptance: OpenClaw configured only from the printed snippet answers a test-channel prompt via a door-served codex or claude seat, locally and via a mesh-dialed door on a second host; no OpenClaw code change.

Conductor verify: `go test ./pkg/cligw/... ./pkg/llmgw/... ./pkg/fleet/...` in yoke, command golden for generic/OpenClaw/Codync blocks, and raw live local plus paired-host OpenClaw channel transcript against a claimed door after S20 is live. Own llm recipe/CLI files; coordinate S1 model CRUD and S20 gateway examples, no edits to their files. Commit with `Agent:` trailer.

Manager review 2026-10-11 07:39 UTC: source `3920e98` merged via guarded yoke weave #80 after independent Mac `GOWORK=off go test ./pkg/broker ./pkg/cligw/...` PASS (`/tmp/s418-mac-s5-3920e98.log`). The original S5 remains blocked on a live local and paired-host OpenClaw channel transcript after S20 and the service monitor/exposure paths are ready. Weave's mechanical done state was reopened; no full acceptance is claimed.

Manager acceptance 2026-10-11 12:43 UTC: exact OpenClaw 2026.9.8 isolated Mac Gateway used the unedited `bashy llm env --client openclaw` provider output from Bashy `5141d40a` and persisted a real TUI user turn plus assistant `S418-LOCAL-WEBCHAT-PONG` with provider `bashy`, model `L4` (redacted transcript `/tmp/s418-s5-local-redacted.json`). A second isolated OpenClaw Gateway on DO used the unedited `BASHY_LLM_PORT=24558 bashy llm env --client openclaw --via mesh` output over an authenticated DO-to-dev-host Outpost mesh listener and persisted a real TUI assistant `S418-MESH-SNIPPET-PONG` with the same provider/model (`/tmp/s418-s5-mesh-redacted.json`). The manager inspected both raw redacted transcript extracts and the earlier source/focused test; temporary Gateways, mesh route/listener, homes and tokens were cleaned, and primary DO services stayed healthy. S5's local and second-host test-channel acceptance is met. This does not claim a Telegram/Discord account or S6 ingress.
