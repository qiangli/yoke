---
id: 3f850f88f82e
kind: chore
title: Restore gofmt gate on existing ACP and llmgw files
seq: 1994
status: done
priority: p1
created: 2026-10-12T15:26:15.583596Z
assignee: s9999-fmt
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T15:43:14.401942Z
closed_by: s9999-maintenance-manager
---

Manager remote fmtcheck on candidate c159f1e stops before crossvet: pkg/chat/acp_adapter_test.go and pkg/llmgw/responses/translate.go whitespace alignment. Apply gofmt to precisely these files in isolated Yoke workspace, no semantic edits; do not baseline or bypass fmtcheck. Acceptance existing scripts/fmtcheck.sh exits0; no new tests for whitespace. NO full crossvet/build/container on Dragon; manager remote gates after integration. Commit proper Sprint9999/Story/Story-ID; preserve generated KB. Raw failure /tmp/s9999-yoke-final-gate.json.
