---
id: 894f429b0b6a
kind: refactor
title: S584.A3 migrate per-tool Go branches to adapters (launch/unsafe, cligw+streamdecode, delegate_guard auth, booth config dir, sdlc/detect, defaults, install-agent)
seq: 3058
status: assigned
priority: p2
labels:
    - agent-cli
created: 2026-11-07T03:48:25.7832Z
assignee: codex-gpt6-sol
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
---

Depends A1/A2; sequence after S575 S1. One commit per area; remove a Go table only after adapter fixtures prove identical behavior. Add shrink-only ratchet counting third-party tool-name literals in non-test Go under yoke/bashy.
