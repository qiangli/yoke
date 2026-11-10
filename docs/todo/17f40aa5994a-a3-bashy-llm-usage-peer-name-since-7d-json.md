---
id: 17f40aa5994a
kind: feature
title: A3 bashy llm usage [--peer NAME] [--since 7d] [--json]
seq: 3070
status: todo
priority: p1
created: 2026-11-10T01:04:30.341071Z
sprint: 602
sprint_id: 945d80da-6344-5fa1-8afb-08d6cac5d7a0
sprint_title: 'Metered shared LLM access: per-peer usage, cost and limits on the LLM door (provider-agnostic)'
---

Per-principal rollup over the door audit.jsonl: requests, prompt/completion/cached tokens, cost_usd, unpriced flag; text table + --json (the one source for reports, alarms and the usage page). Expose the rollup as a Go func reused by A4. Register under the llm verb tree beside peer (pkg/broker cmd.go). Depends on A1+A2 record fields. Tests: unit over a fixture audit log. Testing (operator rule): unit tests plus at most one minimal integration test in this story; full suites only at sprint close or release.
