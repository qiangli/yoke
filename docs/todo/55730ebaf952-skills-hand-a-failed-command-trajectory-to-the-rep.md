---
id: 55730ebaf952
kind: feature
title: 'skills: hand a failed command trajectory to the repair loop (telemetry triggers self-healing)'
seq: 3089
status: todo
priority: p2
labels:
    - skills
    - execlog
    - otel
created: 2026-11-10T20:06:28.315941Z
sprint: 611
sprint_id: 7de44c27-d277-58b6-b42d-e5fabc9d668f
sprint_title: 'OTel logs signal: wire outpost''s slog bridge and drain the bashy span spool into the OTel log store'
---

skill run --adapt --repair-agent (pkg/skills/adapt.go) is the one real self-healing mechanism: a headless agent proposes steps, the original contract and effect cap gate acceptance, the fix lands as a host-local overlay reused by every agent brand. It is triggered only by a skill contract failing and reads neither execlog nor spans. Scope: a 'repair from history' entry point. Given an episode (or --since window), build the failure trajectory from execlog (failed template, the commands that followed, whether one later succeeded, per execlog.Transitions) and, when the failing command maps to a registered skill or a shaped command (bashy shape), invoke the same adapt loop with the trajectory as the repair brief; when it maps to nothing, emit a kb candidate note (--ring host, kind gotcha) addressed by episode and template, never the raw records (the tokb pipe rule). Explicit invocation first (bashy skill repair --episode E); no automatic background repair in this story. Verify: a skill made to fail in a sandbox, followed by a manual fix in the same episode, yields an accepted overlay on the next skill repair --episode run; a non-skill failure yields exactly one candidate kb note.
