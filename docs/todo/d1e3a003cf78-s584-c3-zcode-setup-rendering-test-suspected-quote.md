---
id: d1e3a003cf78
kind: test
title: S584.C3 zcode setup rendering test (suspected quoted base_url inside heredoc JSON)
seq: 3054
status: done
priority: p1
labels:
    - agent-cli
created: 2026-11-07T03:48:25.218238Z
assignee: claude-opus5.5-s584
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
closed: 2026-11-07T04:13:31.708328Z
closed_by: claude-opus5.5-s584
---

SetupScript shell-quotes {base_url}; zcode.yaml interpolates it inside an unquoted heredoc JSON string, likely producing literal quotes in provider_config.json. Add a rendering test executing the setup; fix the template if confirmed. Spec: docs/sprint-agent-cli-adapters-plan.md
