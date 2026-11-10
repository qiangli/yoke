---
id: 0d43fd1f9d65
kind: feature
title: 'otelquery: answer failed/bounds/guessed/why-slow from the local stores when the stack has no exec spans'
seq: 3087
status: todo
priority: p2
labels:
    - otel
    - telemetry
    - execlog
created: 2026-11-10T20:06:28.030242Z
sprint: 611
sprint_id: 7de44c27-d277-58b6-b42d-e5fabc9d668f
sprint_title: 'OTel logs signal: wire outpost''s slog bridge and drain the bashy span spool into the OTel log store'
---

Today every bashy otel verb queries the OTel trace and metrics stores only (pkg/otelquery/store.go, summary.go: name:exec*, name:bound.*, name:value.*). bashy exec spans live in the file spool and the execlog, so for a bashy session the verbs return 0 matches while the spool held 1,677 error spans on 2026-11-10 and graph history --failed answers the same question from execlog. Scope: one query path, two stores. When the stack is unreachable or returns zero exec rows for the window, fall back to the spool (jsonline, same field names) and, for failed, to execlog (template, exit, duration, episode); label the envelope backend as spool|execlog so an agent can tell a quiet store from an unreachable one (the client already separates those two facts). Keep the one-hour default window and the row caps. Verify: with otel-stack stopped, bashy otel failed --since 6h lists the same failures as bashy graph history --failed --since 6h. Companion to the spool drain story on this sprint; independent of it.
