---
id: 9e9a02266c05
kind: feature
title: A2 one LLM price table in yoke; cost_usd + unpriced per door audit record
seq: 3069
status: todo
priority: p1
created: 2026-11-10T01:04:30.207083Z
sprint: 602
sprint_id: 945d80da-6344-5fa1-8afb-08d6cac5d7a0
sprint_title: 'Metered shared LLM access: per-peer usage, cost and limits on the LLM door (provider-agnostic)'
---

New stdlib-only package pkg/llmprice seeded from ycode internal/runtime/usage/pricing.go (input/output/cache-write/cache-read per M). Add Bedrock model ids with id normalization (strip region/vendor prefix such as us.anthropic., zai.). Lookup returns known=false for unknown models: the door never falls back to another model's price. Broker.finish writes cost_usd and unpriced (local models such as ollama are unpriced). ycode switching to this package is a separate later follow-up (no ycode change here). Tests: unit (pkg/llmprice, pkg/broker). Depends on A1 for cached tokens/upstream model; the table package can start in parallel. Testing (operator rule): unit tests plus at most one minimal integration test in this story; full suites only at sprint close or release.
