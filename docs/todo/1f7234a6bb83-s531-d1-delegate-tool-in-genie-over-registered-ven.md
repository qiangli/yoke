---
id: 1f7234a6bb83
kind: feature
title: S531.D1 `delegate` tool in genie over registered vendor CLI commands
seq: 2888
status: todo
priority: p0
labels:
    - genie
created: 2026-10-29T07:01:15.230273Z
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
---

Spec: dhnt umbrella plan §D0 (decided f7d071321). Depends: D0 (accepted). Repo: yoke (pkg/chat delegate verb); genie allow-policy in ycode/examples/genie/agent.yaml is a follow-up slice under this story.

Scope (yoke/pkg/chat/delegate.go + agentlaunch as needed): harden the EXISTING `bashy delegate` verb as genie's delegation primitive — no new verb, no second model-facing tool.
- `--json` bashy-chat-v1 envelope gains: receipt (attest id when run under an attest envelope, else empty), identity (`genie-delegate:<tool>-<model>` when caller is genie, detected via env BASHY_DELEGATE_CALLER=genie; else empty), exit, error.kind ∈ {auth,timeout,idle,budget,denied,vendor}; non-zero exit for every error kind.
- `--idle-timeout` (output-idle watchdog, reuse yoke/pkg/agentpty IdleTimeout semantics) alongside existing --timeout; both fail closed.
- `--budget-usd` / token cap: fail closed with error.kind=budget when exceeded or unmeasurable for a required cap.
- Child env scrub: drop ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY, GOOGLE_API_KEY, and similar metered keys; never read vendor credential files. Vendor not logged in → fast error.kind=auth with clear message. No metered fallback.
- `--allow tool:model[,..]` (or env BASHY_DELEGATE_ALLOW) gate: target outside allow → error.kind=denied before launch. Empty allow = no restriction ONLY when caller is not genie; caller=genie with empty allow → denied.
Acceptance: unit red/green with a fake vendor binary per adapter (claude, codex): success envelope; timeout/idle/budget/denied each fail closed with kind; auth fast-fail; metered env absent in child. `go test ./pkg/chat/...` green. Public repo: describe mechanism generically in commits; no hostnames.
