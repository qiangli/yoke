---
id: 27cdbb239461
kind: feature
title: 'Follow-up (S584): AWS Bedrock via the pooled LLM gateway (bearer upstream) — replaces native Bedrock client'
seq: 3061
status: todo
priority: p2
labels:
    - agent-cli
created: 2026-11-07T03:48:26.212099Z
---

Replaces S584.B2 (native Converse/SigV4 client). KISS: Bedrock exposes an OpenAI-compatible surface (bedrock-runtime .../openai/v1: Chat Completions for all models, Responses for OpenAI models; bedrock-mantle /v1 adds /models + server tools) with a single bearer key, so the pooled LLM service proxies it like any other upstream. Then every door client (door-* models -> 127.0.0.1:24556) reaches Bedrock: claude, codex, opencode, zcode, genie/ycode, and agy via its gateway setting. muse cannot (vendor endpoint only).

Scope: (1) gateway upstream config for Bedrock in the hosted pooled service (private; public side uses only the documented wire protocol); (2) key custody: short-term Bedrock keys (<=12h, region-bound) refreshed by the service (e.g. aws-bedrock-token-generator from an IAM role), never a pasted long-term key; (3) public seeds: door model records for chosen Bedrock models; agy door agent if its gateway mode verifies; (4) llmbudget: metered Bedrock plan + prices so pooled spend is accounted.

Caveats: cross-region inference profiles and Guardrails only on bedrock-runtime; non-OpenAI models are Chat Completions only. Verify live on a test host with a real key. Research: docs/sprint-agent-cli-adapters-plan.md.
