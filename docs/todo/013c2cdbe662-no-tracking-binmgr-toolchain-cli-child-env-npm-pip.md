---
id: 013c2cdbe662
kind: task
title: 'No-tracking: binmgr toolchain/CLI child env (npm, pip/uv, go, dotnet, pwsh, bun, gh, rclone, helm...) - telemetry and update checks off'
seq: 2000
status: done
priority: p1
created: 2026-10-12T17:49:02.746998Z
assignee: muse-spark1.2
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-12T18:00:31.96181Z
closed_by: s434-manager
---

Sprint 434, rule (2) NO THIRD-PARTY TRACKING, config-only, today. Source: docs/bashy-supply-chain-audit.md (umbrella dev 9a55d5ed9), yoke/external wrappers + package registries.
Scope: child environments bashy sets when it runs binmgr toolchains/CLIs in yoke/external/*: node/npm/corepack/pnpm (npm_config_update_notifier=false, npm_config_fund=false, COREPACK/pnpm update checks), python/uv/pip (PIP_DISABLE_PIP_VERSION_CHECK=1, uv equivalents), go (GOTELEMETRY=off or go telemetry off for the bashy-managed toolchain), dotnet (DOTNET_CLI_TELEMETRY_OPTOUT=1, DOTNET_NOLOGO), pwsh (already: verify ChildEnv applied on every launch), bun (DO_NOT_TRACK=1), gh (GH_NO_UPDATE_NOTIFIER=1, GH_TELEMETRY off if present), rclone/helm/mise/doctl/tofu/rustup update checks. Prefer one small shared helper if wrappers have none; DO_NOT_TRACK=1 is a good default for all.
Do NOT edit yoke pkg/binmgr verification or pkg/fleet (S433 owns them). Only yoke/external/* wrappers (and a tiny shared env helper under yoke/external if needed).
Acceptance: each var cited from upstream docs; unit test asserting the child env contains the vars for at least node, python, go, dotnet, pwsh; focused go test for touched packages passes. Commit in yoke with Sprint/Story/Story-ID trailers.
