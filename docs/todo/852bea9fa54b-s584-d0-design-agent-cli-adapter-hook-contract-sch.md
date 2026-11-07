---
id: 852bea9fa54b
kind: doc
title: 'S584.D0 design: agent-CLI adapter hook contract, schema additions, trust model, Mechanism->seed decision'
seq: 3055
status: assigned
priority: p1
labels:
    - agent-cli
created: 2026-11-07T03:48:25.358659Z
assignee: claude-opus5.5-s584
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
---

Write docs/agent-cli-adapters.md: thin Go core (contract, rings, pinning, firewall, unsafe guard on final argv/env, containment, named generic decoders, builtin flag for genie/ycode) + per-tool YAML + optional Bash# .bsh hooks (setup PROVIDER MODEL, auth-check, probe, install, decode-event, detect) with script/skill duality; hash pinning + shape approval for non-embedded adapters; closes C5 open point (third-party tool contracts become seeds). Operator review gate before A-stories.
