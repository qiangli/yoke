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

S602 known limit: PrincipalUsage.unpriced is OR-ed per principal, so a principal mixing priced and unpriced models is checked only against the token cap by the usage alarm. Add priced_tokens/unpriced_tokens (keep existing fields; schema stays bashy-llm-usage-v1 with additive fields) in RollupUsage and the --json report. Agent: codex gpt6-sol. Tests: unit only in the story; full suites at sprint close (operator rule). Commit trailers one per line.
