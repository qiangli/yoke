---
id: 12e9009d3244
kind: feature
title: 'telemetry: drain the bashy span spool into the OTel log store so exec spans are queryable in the stack'
seq: 3086
status: todo
priority: p2
labels:
    - otel
    - telemetry
created: 2026-11-10T19:59:02.255551Z
sprint: 611
sprint_id: 7de44c27-d277-58b6-b42d-e5fabc9d668f
sprint_title: 'OTel logs signal: wire outpost''s slog bridge and drain the bashy span spool into the OTel log store'
---

pkg/telemetry/spool.go writes every bashy exec span to ~/.agents/otel/spool/spans.jsonl in the OTel log store's native jsonline format, chosen so the file imports with the documented curl form (/insert/jsonline?_stream_fields=service). No code or service runs that import: the stack's the OTel trace store holds outpost HTTP spans only, the OTel log store holds nothing, and bashy otel failed/why-slow/bounds query the stack only (pkg/otelquery/store.go), so exec spans are never reachable through the verbs. The spool itself keeps hours, not days (64 MiB soft bound, compacted to the newest half; observed 24 MB / 15 h on 2026-11-10). Scope: a drain in the otel-stack service (yoke/external/otel/stack) or a bashy otel verb that imports the spool into /logs idempotently across restarts (track a byte/record watermark beside the spool, never rename or truncate the spool, honour the never-rename rule), keep the file-first guarantee when the stack is down, and make an empty the OTel log store with the stack up visible as a bound in bashy otel bounds. Verify: a spooled exec span is returned by bashy otel failed after the drain. Companion to the outpost slog-bridge bug on the same sprint. Finding recorded 2026-11-10 in umbrella docs/bash-command-invocation-counts.md.
