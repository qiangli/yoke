---
id: c8283ebb3f9d
kind: feature
title: 'S5 Synergy 1: agent as LLM service - bashy llm env --client openclaw|codync|generic'
seq: 1867
status: todo
priority: p1
labels:
    - yoke
    - bashy
created: 2026-10-11T03:51:51.724068Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Any agentic tool that takes an OpenAI or Anthropic base URL uses a fleet agent served by the bashy llm door (cligw, port 24556) as its brain. Add bashy llm env --client NAME printing the ready provider block for that client (OpenClaw provider config, generic OpenAI env); a model add example registers an external OpenAI-compatible gateway as a model. Sphere reach is configuration only: the door is shared across hosts via outpost mesh service add llm / mesh dial (Sprint 308 MVP); llm env --client gains --via mesh printing the dialed local address. Acceptance: OpenClaw configured only from the printed snippet answers a test-channel prompt via a door-served codex or claude seat, locally and via a mesh-dialed door on a second host; no OpenClaw code change.

Conductor verify: `go test ./pkg/cligw/... ./pkg/llmgw/... ./pkg/fleet/...` in yoke, command golden for generic/OpenClaw/Codync blocks, and raw live local plus paired-host OpenClaw channel transcript against a claimed door after S20 is live. Own llm recipe/CLI files; coordinate S1 model CRUD and S20 gateway examples, no edits to their files. Commit with `Agent:` trailer.
