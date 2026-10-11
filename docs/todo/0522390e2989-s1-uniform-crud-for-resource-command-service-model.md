---
id: 0522390e2989
kind: feature
title: 'S1 Uniform CRUD for resource/command/service/model/tool/agent/skill: YAML round-trip and --json'
seq: 1863
status: done
priority: p1
labels:
    - yoke
    - bashy
created: 2026-10-11T03:51:42.16684Z
weave: 79
assignee: codex-gpt6-luna
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T07:18:31.539263Z
resolution: fixed
closed_by: codex-gpt6-luna-a
---

Every extension kind supports add -f FILE.yaml (and - for stdin), show --yaml, set, rm, list --json and verify with the same shape; every verb is already an MCP tool via bashy mcp. Seven kinds: resource, command, service (S15, docs/bashy-service-design.md), model, tool, agent, skill. Acceptance: one matrix test proves show --yaml piped to add -f - is identity for all seven kinds; each gap found is fixed in yoke/pkg/fleet, the resource registry or the service unit store. Spec docs/agent-ecosystem-synergy.md (extension contract). Regression: cd yoke and go test ./pkg/fleet/...; bashy make test.

Conductor 2026-10-11: S15 and S2 fleet contracts are merged. Own the seven-kind CRUD surface in `pkg/fleet` and its matrix test; coordinate with the S4 approval worker before editing `pkg/chat` or `pkg/acp`. Verify `go test ./pkg/fleet/...` and direct CLI add/show/set/list/rm/verify for each of the seven kinds; run `bashy make test` on a claimed native venue after integration. Commit with `Agent:` trailer.
