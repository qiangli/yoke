---
id: e647a76de67f
kind: bug
title: 'bashy secret: exec NAME -- cmd, refuse bare vault names in env templates (did you mean @NAME), ls <prefix>, ''/'' names, convention help'
seq: 2044
status: done
priority: p1
created: 2026-10-24T09:03:36.128545Z
assignee: codex-gpt6-astra
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-24T19:09:22.891824Z
closed_by: s497-manager
---

Operator 2026-10-24 (scheduled now; this cost the Lane 2 session time): a conductor wrote `NAME=NAME` instead of `NAME=@NAME` in a `bashy secret env` template, the literal names were exported, and every provider token looked revoked. Umbrella todo 571b841f. One repo: yoke (pkg/secrets).
Deliver (small, no redesign):
1. `bashy secret exec NAME [NAME...] -- <cmd> [args]`: fetch each vault secret and run cmd with them in its environment only (env var = the name upper-cased with "/" and "-" -> "_", e.g. do/orbit/agent -> DO_ORBIT_AGENT; `NAME=ENVVAR` form to choose the env var). Unknown name -> non-zero exit with "no such secret: NAME". Never writes values to disk or stdout.
2. `secret env`: when a template RHS is a bare word (no @) that equals an existing vault secret name, refuse with "did you mean @NAME?" (exit non-zero) instead of exporting the literal. (Literal values that are not vault names keep working.)
3. `secret ls <prefix>` filters by prefix (plain string prefix; names may contain "/").
4. Client: path-escape names in GET/DELETE URLs (cloudbox route becomes /secrets/*name in the sibling cloudbox story).
5. Help text: one convention paragraph: names `<provider>/<account>/<purpose>` (e.g. do/orbit/agent, docr/dhnt/push); local secret files only under ~/.ssh/; use `secret exec` for one-off commands.
Tests with an httptest fake vault and temp BASHY_HOME: exec injects only into the child; unknown name fails; template bare-name refusal; prefix ls; names with "/" round-trip. No build outputs in commits. Gate: go test ./pkg/secrets/... + go build -o /dev/null ./... . Commit with this story's trailers.
