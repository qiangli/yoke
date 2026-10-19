---
id: f1e78acc4466
kind: bug
title: 'kopia: server argv passes repository-only --no-check-for-updates (service fails to start); use KOPIA_CHECK_FOR_UPDATES=false'
seq: 2007
status: done
priority: p1
labels:
    - supply-chain
created: 2026-10-13T00:34:59.411358Z
assignee: claude-sonnet5.5
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-13T00:40:02.320226Z
closed_by: s434-manager
---

BUG from S434 story 32d44650: yoke external/kopia/kopia.go:177 passes `--no-check-for-updates` (plus --otlp-trace) to kopia's SERVER command, but at v0.23.1 --no-check-for-updates is a repository create/connect flag; `kopia server start` rejects it (qiangli/kopia fork CI no-egress gate failed: server never started). So the managed kopia service fails to start today.
Fix: drop --no-check-for-updates from the server argv; disable update checks with the env var kopia reads at the tag - cli/update_check.go checkForUpdatesEnvar = "KOPIA_CHECK_FOR_UPDATES" resolved through c.EnvName(...) (verify whether EnvName adds a prefix at v0.23.1 and use the exact resulting name) = false, set in the managed child env (external/internal/serviceenv or the kopia wrapper's ChildEnv, alongside the existing no-tracking env). Verify --otlp-trace is accepted by `server start` at the tag too (cite source); if not, fix the same way.
Test: unit test that builds the server argv + env and asserts no repository-only flags and KOPIA_CHECK_FOR_UPDATES=false; plus a local live check on dev-host if kopia v0.23.1 is cached via binmgr: `kopia server start --help` accepts every flag we pass (paste). Gate: go vet + go test -count=1 ./external/kopia/... ./external/internal/... green. Update docs/service-runtime-privacy.md kopia row. Commit with trailers, sprint submit with SHA + gate.
