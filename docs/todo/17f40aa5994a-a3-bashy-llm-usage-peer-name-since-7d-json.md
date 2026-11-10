---
id: 17f40aa5994a
kind: feature
title: A3 bashy llm usage [--peer NAME] [--since 7d] [--json]
seq: 3070
status: done
priority: p1
created: 2026-11-10T01:04:30.341071Z
weave: 23
assignee: codex-gpt5.6-sol
sprint: 602
sprint_id: 945d80da-6344-5fa1-8afb-08d6cac5d7a0
sprint_title: 'Metered shared LLM access: per-peer usage, cost and limits on the LLM door (provider-agnostic)'
closed: 2026-11-10T02:07:23.233026Z
closed_by: claude-opus5.5-s602
---

Per-principal rollup over the door audit.jsonl: requests, prompt/completion/cached tokens, cost_usd, unpriced flag; text table + --json (the one source for reports, alarms and the usage page). Expose the rollup as a Go func reused by A4. Register under the llm verb tree beside peer (pkg/broker cmd.go). Depends on A1+A2 record fields. Tests: unit over a fixture audit log. Testing (operator rule): unit tests plus at most one minimal integration test in this story; full suites only at sprint close or release.
Manager note (A1 merged c0ff5c74): audit Record now has cached_tokens and upstream_model. A2 (in flight, branch agent/weave-issue-21 commit 36817dc0) adds cost_usd (float, omitted when unpriced) and unpriced (bool); code the rollup against those JSON names now. Commit trailers each on its own line, no blank lines between.
