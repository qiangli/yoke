---
id: 27cdbb239461
kind: feature
title: S586.1 AWS Bedrock as an outpost pool upstream (host-minted short-term key)
seq: 3061
status: todo
priority: p2
labels:
    - agent-cli
created: 2026-11-07T03:48:26.212099Z
sprint: 586
sprint_id: 0d040542-da67-5fa5-917b-6d3a22bdfe3c
sprint_title: AWS Bedrock via the pooled LLM gateway (bearer upstream)
---

Replaces S584.B2 (native Converse/SigV4 client). Re-scoped 2026-11-07: the Bedrock upstream lives in OUTPOST (public), not the hosted service. AWS credentials stay on the operator's host; the pool stays a thin relay.

Fit (inspected, not assumed):
- outpost already supplies the pool: the ollama-pool unit pushes its model list on the registry wire (outpost internal/agent/ollama/types.go RegistryPushPayload, watcher.go push), and the pool sends OpenAI-compat requests through the outpost `ollama` app proxy (counter.go counts /v1/chat/completions there).
- precedent for a non-Ollama OpenAI-compat upstream on that wire: internal/agent/clusterllm (cluster_llm_endpoint/api_key, inert on failure, additive only).
- Bedrock short-term key = "bedrock-api-key-" + base64 of a SigV4-presigned bedrock CallWithBearerToken URL (<=12h, region-bound); yoke/pkg/provider/aws already has PresignURL + credentials + STS. No AWS SDK.
- clients reach the pool by base_url on its OpenAI-compat /v1 (bashy docs/why-bashy.md "pooled endpoint"); no public seed points at it yet; door-* seeds are retired and are not the route.

Scope (one story, MVP):
1. outpost: Bedrock upstream, off by default (config: region, model list; credentials from the host's AWS profile/role). Mint + refresh the short-term key in memory (never persisted, never logged, redacted in SafeView); advertise the chosen models on the registry push; the proxy routes those model names to bedrock-runtime /openai/v1/chat/completions with the bearer injected; everything else is unchanged. Fail inert like clusterllm.
2. yoke seeds: pool-routed Bedrock models (openai-compat, pool base_url, no family) + agents for genie and opencode/zcode; plans/aws-bedrock.yaml metered + cost_micro with as_of/source.
3. Live on a test host: genie + one of opencode/zcode through the pool with an outpost-minted key.

Out of MVP: Responses API (not on the pool wire today; codex keeps the S584 native amazon-bedrock route), agy (gateway mode unverified), muse (Meta endpoint only), cross-region profiles/Guardrails beyond what bedrock-runtime gives for free. If the pool does not route an advertised non-Ollama model name, that minimal change is filed in the private repo, referenced here by wire contract only.

Gates: outpost make test; yoke GOWORK=off go test ./pkg/fleet/... ./pkg/llmbudget/...; native agents render byte-identical; bashy make test + test-bash on the pin bump (remote test host).
