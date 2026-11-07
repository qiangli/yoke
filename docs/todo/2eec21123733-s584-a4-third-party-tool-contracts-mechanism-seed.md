---
id: 2eec21123733
kind: refactor
title: S584.A4 third-party tool contracts Mechanism->seed; BASHY_FLEET_SEEDS=off leaves only genie/ycode
seq: 3059
status: assigned
priority: p2
labels:
    - agent-cli
created: 2026-11-07T03:48:25.924292Z
assignee: agy-gemini3.8-flash
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
---

Depends A3. pkg/fleet/kinds.go:109 tools Mechanism:true; split builtin (ycode/genie) from seeded third-party contracts; regression test.
