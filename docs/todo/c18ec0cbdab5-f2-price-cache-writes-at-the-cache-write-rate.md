---
id: c18ec0cbdab5
kind: enhancement
title: F2 price cache writes at the cache-write rate
seq: 3075
status: todo
priority: p3
created: 2026-11-10T03:47:55.971778Z
sprint: 607
sprint_id: d3d7f687-3414-576c-ad29-b45dfc879257
sprint_title: 'Metering follow-ups: one price table, honest cost and alarms, member go-live guards'
---

S602 known limit: the audit record folds Anthropic cache_creation_input_tokens into prompt_tokens and Broker.price bills them at the input rate. Record cache_write_tokens beside cached_tokens (parseUsage in pkg/broker/audit.go, cligw Usage) and pass CacheWrite to llmprice.Cost. Agent: codex gpt5.6-sol. Tests: unit only in the story; full suites at sprint close (operator rule). Commit trailers one per line.
