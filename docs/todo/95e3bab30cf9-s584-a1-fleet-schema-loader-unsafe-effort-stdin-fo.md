---
id: 95e3bab30cf9
kind: feature
title: 'S584.A1 fleet schema + loader: unsafe/effort/stdin_format/decoder/auth/config_dir_env/builtin; wire env_markers; seeded defaults record'
seq: 3056
status: todo
priority: p2
labels:
    - agent-cli
created: 2026-11-07T03:48:25.500737Z
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
---

Depends D0. Add fields to pkg/fleet/types.go; read existing cli.launch.env_markers (declared, never read); seeded defaults record (weave default fleet genie,claude,codex,opencode,agy,muse,zcode; role defaults; meet secretary; sdlc fallback conductor).


Defaults record binds each favored tool to its native default model/subscription; alternative LLM providers are optional per-agent bindings.
