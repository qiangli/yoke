---
id: e79fdc3ee3c9
kind: enhancement
title: 'F3 llm usage: split priced and unpriced tokens per principal'
seq: 3076
status: todo
priority: p2
created: 2026-11-10T03:47:56.108312Z
sprint: 607
sprint_id: d3d7f687-3414-576c-ad29-b45dfc879257
sprint_title: 'Metering follow-ups: one price table, honest cost and alarms, member go-live guards'
---

S602 known limit: PrincipalUsage.unpriced is OR-ed per principal (pkg/broker/usage.go), so a principal mixing priced and unpriced models is checked only against the token cap by the usage alarm.

Sprint 607 F3, scope confirmed 2026-11-10 (plan: dhnt-docs docs/sprint-607-master-execution-plan.md): add priced_tokens (prompt+completion of records with unpriced false) and unpriced_tokens (the same for unpriced true) to PrincipalUsage and the --json report, additive; keep every existing field and the unpriced flag's meaning (true when any record is unpriced); schema stays bashy-llm-usage-v1; no flag changes to `llm usage`. Merge before F2 (both touch pkg/broker). Gate: go test ./pkg/broker/... Agent: codex gpt6-sol. Work in a weave workspace, never the shared checkout. Tests: unit only in the story; full suites at sprint close (operator rule). Commit trailers one per line.
