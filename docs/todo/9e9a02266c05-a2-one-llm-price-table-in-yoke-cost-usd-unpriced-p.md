---
id: 9e9a02266c05
kind: feature
title: A2 one LLM price table in yoke; cost_usd + unpriced per door audit record
seq: 3069
status: doing
priority: p1
created: 2026-11-10T01:04:30.207083Z
weave: 21
assignee: genie-glm-5.3
sprint: 602
sprint_id: 945d80da-6344-5fa1-8afb-08d6cac5d7a0
sprint_title: 'Metered shared LLM access: per-peer usage, cost and limits on the LLM door (provider-agnostic)'
---

Delivered on branch agent/weave-issue-21: pkg/llmprice (stdlib-only price table, USD per M for input/output/cache-write/cache-read) seeded from ycode internal/runtime/usage/pricing.go with two corrections over the seed: (1) unknown models return Known=false with zero rates and NO fallback price - the seed answered unknowns with Claude Sonnet's $3/$15 rate; (2) Bedrock ids normalize via NormalizeModel (strips cross-region us./eu./apac./global., single-region us-east-1., vendor anthropic./zai./openai./amazon./meta./deepseek., provider paths moonshot//zai-coding-plan//amazon-bedrock/, and ollama :tags; never eats version numbers like glm-5.3). Bedrock rows added for the fleet seeds: us./eu.anthropic.claude-opus-4-8 (priced as the weights, per models/bedrock-opus4.8.yaml) and zai.glm-5. openai.gpt-6-sol is deliberately UNPRICED - no per-M rate is documented anywhere in this repo (its native route is a subscription seat; cost_micro is a relative weight, not a price). Same rule for glm-5.3-flash, kimi-k2.7-code-highspeed, kimi-k3, muse-spark, gpt-oss: absent rather than invented. Broker.finish now calls b.price() writing cost_usd / unpriced on every audit Record: local engine models (BackendLocal/ollama) are unpriced even when the name matches a table row; unknown ids are unpriced; tokenless refusals (queue-full, memory, malformed) carry neither field so a refusal never reads as a free request; Options.Pricer is the override seam (nil = llmprice.Cost, and a hook returning ok=false is honored as unknown). ycode untouched (separate follow-up). NOTE for A1/A3: cached tokens and upstream model id are NOT wired into the cost yet - Cost currently prices prompt+completion only, because the audit Record carries no cache buckets until A1 lands; once A1's fields exist, extend price() to pass CacheWrite/CacheRead and to price rec's upstream model instead of the agent alias (a sticky CLI record's Model is the agent name today, which correctly prices as unpriced). Verification: go test -count=1 ./pkg/llmprice/ ./pkg/broker/ (ok, incl. -race on llmprice), go vet both (clean), go build ./cmd/yoke (ok). No full suite run (operator rule: unit + one minimal integration, TestAuditFileCarriesCostFields is the one integration test).
