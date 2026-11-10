---
id: 742fd57377db
kind: enhancement
title: 'telemetry: exec error spans carry enough context to explain a failure without the transcript'
seq: 3088
status: todo
priority: p2
labels:
    - otel
    - telemetry
created: 2026-11-10T20:06:28.174173Z
sprint: 611
sprint_id: 7de44c27-d277-58b6-b42d-e5fabc9d668f
sprint_title: 'OTel logs signal: wire outpost''s slog bridge and drain the bashy span spool into the OTel log store'
---

An exec span today records cmd.name/argv/argc/cwd/exit_code/duration and, on error, status_message 'exit N' and nothing else (pkg/telemetry/exec.go). agent.principal is present on about a quarter of spans; no episode id, no parent episode, no stderr. Scope, kept disjoint from Sprint 590 (TRACEPARENT, gen_ai.turn parenting, benign-probe classification stay there): 1) stamp bashy.episode and bashy.parent_episode (the execlog identity, already in BASHY_PARENT_EPISODE) and agent.principal on every exec span, not only on some paths; 2) on non-zero exit attach a scrubbed, bounded stderr excerpt (reuse execlog.Scrub; cap ~512 bytes; record the cap as a bound.hit when it binds) and error.type; 3) record the execlog template on the span so spool and execlog join on the same key. Verify: an exec span for a failing go test carries episode, principal, template and an excerpt, and jq over the spool explains the failure without the agent transcript.
