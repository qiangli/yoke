---
id: 580c606a7558
kind: feature
title: 'A1 usage completeness: cached tokens + upstream model in the LLM door audit record'
seq: 3068
status: assigned
priority: p1
created: 2026-11-10T01:04:30.065973Z
weave: 22
assignee: codex-gpt6-sol
sprint: 602
sprint_id: 945d80da-6344-5fa1-8afb-08d6cac5d7a0
sprint_title: 'Metered shared LLM access: per-peer usage, cost and limits on the LLM door (provider-agnostic)'
---

Add cached_tokens and upstream_model to pkg/broker audit Record. parseTokens (audit.go): OpenAI prompt_tokens_details.cached_tokens, Anthropic cache_read_input_tokens/cache_creation_input_tokens; on Anthropic SSE keep input_tokens from message_start (today the last block, message_delta, wins). cligw: carry Usage.CachedInputTokens and the launched model id through sticky_worker.go (~:200) and the broker's re-encode after delegateCLI (broker.go ~:811). Passthrough route: register a data-only zero-harness agent (ycode with a one-shot harness derived from ycode/examples/agent-zero.yaml: no tools/context/history, one llm.call, provider from the model registry) and one fleet agent binding per Bedrock model (e.g. zero-bedrock-glm-5 beside agents/ycode-bedrock-glm-5.yaml), so peer requests to Bedrock are metered without agent tool-loop overhead. Verify cligw usageFromEvent reads ycode's usage incl. cache fields; fix the parser if not. Tests: unit (go test ./pkg/broker/... ./pkg/cligw/) + one minimal integration: passthrough agent against a fake OpenAI-compatible endpoint, tokens/cached/model reach audit.jsonl. Testing (operator rule): unit tests plus at most one minimal integration test in this story; full suites only at sprint close or release.
