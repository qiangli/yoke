---
id: 4fb33664d5a0
kind: refactor
title: S584.A3b defaults + name guessing from rosters/catalog (meet/sota/sdlc/cooldown)
seq: 3062
status: assigned
priority: p1
labels:
    - agent-cli
created: 2026-11-07T07:41:46.636243Z
assignee: muse-spark1.3
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
---

Split from A3 (894f429b0b6a) for parallel workers. meet secretary, sota agent, sdlc DefaultConductorAgent/normalizeAgentName, weave_cooldown name guess read rosters/catalog; literals only as seeds-off fallback.
