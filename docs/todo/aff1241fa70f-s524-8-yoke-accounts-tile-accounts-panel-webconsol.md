---
id: aff1241fa70f
kind: feature
title: 'S524.8 yoke: Accounts tile + /accounts/ panel (webconsole)'
seq: 2908
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T09:39:02.913526Z
weave: 690
assignee: agy-gemini3.8-flash
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T10:22:19.612654Z
resolution: fixed
closed_by: agy-gemini3.8-flash-n
---

Part of S524.8 (umbrella story 194956d07ee1, Sprint 524): Accounts tile + /accounts/ panel in the bashy app (yoke/pkg/webconsole). Builds on yoke main (pkg/accounts store + per-account refresh snapshots from S524.2c; adapters cloudbox/llm/digitalocean/aws; S525 principal scoping + Shape panel already merged at c939ec6f).
HARD BOUNDARY (another sprint owns these; do NOT edit): pkg/webconsole host_auth.go, pairing.go, pair_*.go, login.go, gate.go, principal.go, handler.go principal stamping, shaped*.go, the Shape panel/artifact files, pkg/src, pkg/shape, pkg/coopauth. Registration edits (tile list / mux wiring) ARE allowed — keep them to the minimal lines.
Model on an existing panel: panel_leaderboard.go (+ _test.go, _dom_test.go verifydom pattern) and builtin_tiles_dom_test.go.
Scope:
1. NEW panel_accounts.go: GET /accounts/ page; GET JSON APIs list (summary rows), detail by id, resources, usage?period=day|week|month — all read the pkg/accounts store + snapshots ONLY (never call adapters); POST refresh (id or all) runs the bounded refresh, protected exactly like other state-changing panel POSTs (same auth + CSRF/origin protection the console already uses; find the existing helper and reuse it).
2. Accounts tile in the builtin tiles list.
3. UI: summary rows (avatar or initials, alias, provider/category, team/profile, status, quota/reset, spend with currency, freshness age), filter/search by provider/category/team/status, expand detail (profile/dashboard URLs as safe links rel=noopener with http(s) scheme check, IDs, credential REF labels and scopes only, resources, usage history with classification + coverage visible, copyable CLI examples like `bashy account show <id>`). Unavailable metrics render as "unavailable", never 0. Inspection + refresh only; no CRUD.
4. Scoped access: reuse the existing panel scoping/principal resolution (read principal.go, don't edit it).
Tests: Go handler tests (list/detail/resources/usage/refresh, GET never calls an adapter — fake adapter t.Fatal, refresh POST rejected without auth/CSRF) and verifydom DOM tests for summary/detail/filter/empty/loading/error/stale states. Secret sentinel (pkg/accounts/accountstest) absent from every response body.
Gates: go test ./pkg/webconsole/ -run 'Account|Tile' -count=1, then the full ./pkg/webconsole/ package once, go vet ./pkg/webconsole/, go build ./..., GOOS=windows go build ./pkg/webconsole/. Disk is low: no per-run GOCACHE copies. Placeholder names only. Trailers: Sprint: #524, Story: S524.8, Story-ID: 194956d07ee1 (plus this yoke story's full 12-hex id).
