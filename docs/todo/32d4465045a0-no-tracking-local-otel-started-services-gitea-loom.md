---
id: 32d4465045a0
kind: task
title: 'No-tracking + local OTel: started services (gitea/loom, act_runner, seaweedfs, kopia, zot, ollama, victoria stores, headlamp) - phone-home off, OTLP to bashy local collector'
seq: 2001
status: done
priority: p1
created: 2026-10-12T17:49:04.07167Z
assignee: codex-gpt5.6-sol
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-12T18:05:49.650795Z
closed_by: s434-manager
---

Sprint 434, rule (2) NO THIRD-PARTY TRACKING + OTel rule, config-only, today. Source: docs/bashy-supply-chain-audit.md (umbrella dev 9a55d5ed9), service table.
Scope: the config/flags/env bashy writes when it starts these services: loom (Gitea: [cron.update_checker] ENABLED=false, any other phone-home), actrunner, seaweedfs, kopia (update checks, anonymous reports), zot, ollama (yoke/external/ollama; version/update check), victoria-logs/metrics/traces in yoke/external/otel/stack (disable any reporting flags), headlamp (outpost/internal/agent/headlamp; disable analytics if flag exists). For every one that supports OpenTelemetry, ENABLE it and set the OTLP endpoint explicitly to bashy's local collector (localhost, the address yoke/external/otel already uses) - never inherit an arbitrary OTEL_EXPORTER_OTLP_ENDPOINT and never a vendor default.
Do NOT change acquisition/verification (yoke pkg/binmgr, pkg/fleet: S433) and do NOT disable/remove services (operator decision pending). Config only.
Acceptance: per service, flag + upstream citation; unit tests asserting generated config/env (update checker off, OTLP endpoint = local collector); focused go test of touched packages passes. Commit in yoke (and outpost if headlamp touched) with Sprint/Story/Story-ID trailers.
