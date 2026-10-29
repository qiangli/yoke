---
id: 337e229f4491
kind: feature
title: 'S524.2c yoke: cache-only usage/resources reads + one adapter registry'
seq: 2904
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T08:59:31.035662Z
weave: 686
assignee: muse-conductor
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T09:30:59.084347Z
resolution: fixed
closed_by: muse-conductor-j
---

Part of S524.2 (umbrella story 0f9695efe8d8, Sprint 524), slice C (yoke): cache-only reads + one adapter registry. Builds on yoke main (S524.1 store, S524.2a cmd.go, S524.3 cloudbox adapter, S524.5 LLM adapter).
1. Cache-only reads (spec: reads are cache/local-only; only explicit refresh calls adapters). Today `account usage` and `account resources` (pkg/accounts/cmd.go usageCmd/resourcesCmd) call adapter Usage/Billing/Resources live. Change: `account refresh ID|--all` (bounded, sequential, per-adapter timeout) calls Verify/Resources/Usage/Billing where the adapter has the capability and PERSISTS a per-account snapshot in the store (resources, metrics per period, observed_at, last_error; last-good preserved on error — an error updates last_error but keeps the previous resources/metrics). `usage` and `resources` read ONLY that snapshot and show its age (observed_at); never call adapters. Add a cmd_test with a fake adapter whose Usage/Billing/Resources/Verify call t.Fatal, proving list/show/usage/resources never call it; plus a refresh test proving last-good survives an adapter error.
2. One registry: S524.2a added DefaultRegistry()/MustRegisterAdapter; S524.5 added RegisterDefaults(r). Unify to ONE mechanism (keep DefaultRegistry + MustRegisterAdapter; make the LLM adapter registered through it; delete RegisterDefaults or make it a thin wrapper used once). Built-in adapters in subpackages (pkg/accounts/adapters/cloudbox, and the upcoming digitalocean/aws ones) must be registered from a single place that does not create an import cycle (e.g. a small pkg/accounts/builtin package imported by the command constructor or by bashy dispatch). `account discover [--dry-run]` must run every registered Discover-capable adapter, including cloudbox and llm.
Gates: go test -race ./pkg/accounts/... ./pkg/atlas/, go vet same, go build ./..., GOOS=windows go build ./pkg/accounts/... . Secret sentinel in new tests. No bashy edits. Do not edit pkg/webconsole, pkg/src, pkg/shape. Trailers: Sprint: #524, Story: S524.2, Story-ID: 0f9695efe8d8 (plus this yoke story id).
