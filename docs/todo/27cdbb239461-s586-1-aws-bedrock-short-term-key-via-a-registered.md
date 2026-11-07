---
id: 27cdbb239461
kind: feature
title: S586.1 AWS Bedrock short-term key via a registered refresh command (secrets.map !cmd)
seq: 3061
status: doing
priority: p2
labels:
    - agent-cli
created: 2026-11-07T03:48:26.212099Z
sprint: 586
sprint_id: 0d040542-da67-5fa5-917b-6d3a22bdfe3c
sprint_title: AWS Bedrock via the pooled LLM gateway (bearer upstream)
---

Replaces S584.B2 (native Converse/SigV4 client). Re-scoped 2026-11-07 (third pass): no outpost, cligw, pool or tool-contract change. A short-term Bedrock key is minted on the host, at credential resolution, by a registered bashy command.

Fit (inspected):
- Every launch resolves the model's api_key_ref through one path: parent env -> host secrets.map binding -> vault (yoke pkg/secrets resolve.go / projection.go). Door workers (cligw/worker.go:1143), chat, weave and genie (bashy internal/agentos/genie_external.go:124) all use it, once per launch.
- Each tool already projects that one value under its own name: credential_env aws-bedrock -> AWS_BEARER_TOKEN_BEDROCK (claude/codex/opencode), key_env ZCODE_API_KEY (zcode), OPENAI_API_KEY (genie). So any OpenAI-compatible tool gets it with no per-tool work.
- secrets.map RHS today is @vault-ref or a literal (secrets.go binding).

Scope:
1. yoke pkg/secrets: a third binding form, NAME=!CMD ARGS — a host command whose trimmed stdout is the value (AWS credential_process style). argv split on whitespace, PATH lookup, no shell; timeout; non-zero/empty = unbound (launch fails with the existing "cannot authenticate here"); never cached, never logged. Generic: any short-lived token (bedrock, gcloud print-access-token, ...).
2. bedrock-key, a Bash# command (yoke examples/bedrock-key, ~~~go fence, stdlib only): mints bedrock-api-key-<b64 SigV4-presigned CallWithBearerToken URL> (<=12h, region-bound) from env credentials or `bashy aws configure export-credentials`; registered with `bashy commands add`. Host binding: AWS_BEDROCK_API_KEY=!bashy bedrock-key.
3. Seeds: genie Bedrock agent (ycode-bedrock-glm-5) on the existing bedrock-glm-5 model.
4. Live on a test host: genie + one third-party CLI (claude/codex/opencode/zcode) call Bedrock with a minted key — this also confirms the presign payload hash (empty-payload SHA-256, not S3's UNSIGNED-PAYLOAD; signature cross-checked offline against yoke's SigningKey/StringToSign).

Out of MVP: metered Bedrock plan + prices (record when verified live; no price beats a wrong one), agy, muse, pool advertisement.

Gates: yoke GOWORK=off go test ./pkg/secrets/... ./pkg/fleet/... ./pkg/agentlaunch/...; native agents byte-identical; bashy make test + test-bash on the yoke bump (remote test host).
