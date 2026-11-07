---
id: 8db3bf297ab8
kind: feature
title: 'S584.B1 AWS Bedrock: model seeds + per-adapter setup (claude/codex/opencode native, zcode OpenAI-compat, agy gateway, muse unsupported)'
seq: 3060
status: todo
priority: p2
labels:
    - agent-cli
created: 2026-11-07T03:48:26.067892Z
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
---

Models provider aws-bedrock (runtime and mantle base_url, ids per tool). Bearer grant api_key_ref aws-bedrock -> credential_env; extend grants for multi-name SigV4/profile (reuse fleet.AccountSelection). Verify on a test host with a short-term Bedrock key. Research summary in docs/sprint-agent-cli-adapters-plan.md.


Opt-in only: each tool's native default model/subscription stays the default; Bedrock agents are additional seeds selected explicitly, and native agents must render identical argv/env (regression test).
