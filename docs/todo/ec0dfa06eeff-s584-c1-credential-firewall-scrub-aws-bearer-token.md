---
id: ec0dfa06eeff
kind: bug
title: 'S584.C1 credential firewall: scrub AWS_BEARER_TOKEN_BEDROCK; grant-gate AWS_*/CLAUDE_CODE_USE_* passthrough'
seq: 3052
status: done
priority: p0
labels:
    - agent-cli
created: 2026-11-07T03:48:24.933001Z
assignee: claude-opus5.5-s584
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
closed: 2026-11-07T04:13:30.968584Z
closed_by: claude-opus5.5-s584
---

AWS_BEARER_TOKEN_BEDROCK passes secrets.ScrubAgentEnv (pkg/secrets/firewall.go:145; _BEDROCK matches no suffix). AWS_PROFILE, AWS_REGION, CLAUDE_CODE_USE_BEDROCK are not scrubbed either, so a spawned claude can silently run on Bedrock. Fix: add to credentialNames; route provider-selection vars through grants. Verify: firewall tests assert scrubbed unless granted. Spec: docs/sprint-agent-cli-adapters-plan.md


Must not break native defaults: subscription logins (e.g. claude/codex/agy/muse state dirs) are unaffected; only an operator-exported Bedrock selection stops leaking implicitly — it becomes an explicit grant on an opt-in Bedrock agent.
