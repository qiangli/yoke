---
id: c18ec0cbdab5
kind: enhancement
title: F2 price cache writes at the cache-write rate
seq: 3075
status: assigned
priority: p3
created: 2026-11-10T03:47:55.971778Z
weave: 48
assignee: codex-gpt6-sol
sprint: 607
sprint_id: d3d7f687-3414-576c-ad29-b45dfc879257
sprint_title: 'Metering follow-ups: one price table, honest cost and alarms, member go-live guards'
---

S602 known limit: the audit record folds Anthropic cache_creation_input_tokens into prompt_tokens and Broker.price bills them at the input rate, although llmprice.Pricing already carries CacheWritePerM (Claude rows have it; OpenAI rows declare 0).

Sprint 607 F2, scope confirmed 2026-11-10 (plan: dhnt-docs docs/sprint-607-master-execution-plan.md): (1) Record gets cache_write_tokens beside cached_tokens; prompt_tokens stays the whole prompt. (2) parseUsage (pkg/broker/audit.go) returns the cache-write count for Anthropic-shaped JSON and SSE (OpenAI shapes have no write field; zero). (3) Broker.price subtracts cache writes from input and passes CacheWrite to llmprice.Cost. (4) cligw: Usage gets a cache-write field read from cache_creation_input_tokens in usageFromEvent (the zero-harness passthrough is the only Bedrock route and ycode emits that key) and re-encoded beside cached_tokens in backend.go. (5) PrincipalUsage / `llm usage --json` gets cache_write_tokens, additive; schema bashy-llm-usage-v1 unchanged. Touches pkg/broker like F3: merge F3 first and rebase. Gate: go test ./pkg/broker/... ./pkg/cligw/ ./pkg/llmprice/. Agent: codex gpt5.6-sol. Work in a weave workspace, never the shared checkout. Tests: unit only in the story; full suites at sprint close (operator rule). Commit trailers one per line.
