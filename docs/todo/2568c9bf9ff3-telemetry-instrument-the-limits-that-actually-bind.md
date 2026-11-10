---
id: 2568c9bf9ff3
kind: enhancement
title: 'telemetry: instrument the limits that actually bind so otel bounds and guessed stop being empty'
seq: 3090
status: todo
priority: p3
labels:
    - otel
    - telemetry
created: 2026-11-10T20:06:28.45501Z
sprint: 611
sprint_id: 7de44c27-d277-58b6-b42d-e5fabc9d668f
sprint_title: 'OTel logs signal: wire outpost''s slog bridge and drain the bashy span spool into the OTel log store'
---

telemetry.BoundHit and telemetry.Provenance (pkg/telemetry/telemetry.go) have two callers: pkg/chat/coach.go and pkg/sota. The limits that bind an agent session elsewhere are reported as stderr notices or not at all: spool soft/hard bound and compaction, llm quota and budget admission, coordination admission caps, command timeouts (timeout wrapper, 27,695 invocations in the execlog), execlog lost/malformed counts, kb context --budget truncation. Scope: route each of those through BoundHit/Provenance at the producer seam (the status.go pattern: emit on change, never per poll), so bashy otel bounds and guessed answer for a bashy session. Verify: run a kb context --budget 100 and a timeout 1 sleep 5; both appear in bashy otel bounds (after the spool/execlog fallback story) with kind, limit and actual.
