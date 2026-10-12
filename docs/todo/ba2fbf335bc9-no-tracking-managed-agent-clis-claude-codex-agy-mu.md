---
id: ba2fbf335bc9
kind: task
title: 'No-tracking: managed agent CLIs (claude/codex/agy/muse/kimi/opencode) - telemetry, crash reporting, update checks off via fleet tool YAML'
seq: 1999
status: done
priority: p1
created: 2026-10-12T17:49:01.397059Z
assignee: s434-manager
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-12T18:36:37.28105Z
closed_by: s434-manager
---

MANAGER STATUS 2026-10-12: your commit a3b7a643 is MERGED to yoke main as df246d32 and verified (go test ./pkg/fleet green). The ONLY remaining step: run   bashy sprint submit 434 ba2fbf335bc9 -m "yoke main df246d32 (a3b7a643); go test ./pkg/fleet green"   and exit. Make NO code changes and NO commits.

Sprint 434, rule (2) NO THIRD-PARTY TRACKING, config-only, today. Source: docs/bashy-supply-chain-audit.md (umbrella dev 9a55d5ed9), managed CLI table.
Scope: yoke/pkg/fleet/baseline/tools/*.yaml data ONLY (cli.managed.env and the documented config keys) for the managed agent CLIs: claude, codex, agy, muse, kimi-cli, opencode. For each, from upstream docs or source at the pinned version, find every env var/config key that disables telemetry, analytics, crash/error reporting, feedback upload, update checks, and non-essential traffic (e.g. claude DISABLE_TELEMETRY, DISABLE_ERROR_REPORTING, CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC; codex check_for_update_on_startup=false and analytics/otel config; opencode share/telemetry; DO_NOT_TRACK=1 where honoured). Add them to cli.managed.env (or the codex config the launch already writes). If a tool supports OpenTelemetry export, it may only point at bashy's local collector, never a vendor endpoint; leave unset otherwise.
Do NOT edit yoke pkg/binmgr or the pkg/fleet Go verify/launch code (S433 owns it); YAML data only. If a key requires Go code to inject, record it in the story note instead.
Acceptance: KISS - a table in docs/sprint-434-evidence.md (umbrella) or the commit message listing per tool: flag added + upstream citation; yoke go test ./pkg/fleet/... passes (focused, on this host is fine). Commit in yoke with Sprint/Story/Story-ID trailers.
