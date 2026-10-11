---
id: 7387078aaab4
kind: refactor
title: S17 yoke built-in lifecycle conversion and full seeded unit coverage
seq: 1886
status: done
priority: p1
labels:
    - yoke
created: 2026-10-11T07:32:14.745991Z
weave: 82
assignee: claude-opus5.5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T23:22:24.009783Z
closed_by: claude-opus5.5
---

Scoped child of Sprint 418 S17 Story-ID eae40495aac7. Own yoke only: pkg/fleet/baseline/services, pkg/meet/service.go, pkg/sdlc/service.go, pkg/svcd, and yoke tests. Replace duplicated meet/SDLC/svcd detached start/status/stop lifecycle with foreground serve and service unit declarations, preserving all old supported parameters, health and route behavior. Extend seeded units for all yoke-owned built-ins and retained in-process outpost capabilities through the S15 generic provider contract. No service-name branch, old config format, migration alias, separate internal toggle, or fake child process. Do not touch Bashy supervisord (S16), Bash# confirmation (S4 bridge), outpost (S18), or coreutils schedule; those remain separate S17 dependencies. Verify on your private branch with GOWORK=off go test ./pkg/fleet/... ./pkg/service/... ./pkg/meet/... ./pkg/sdlc/... ./pkg/svcd/... and focused live foreground health/list/status smoke; manager owns remote native gate and final S17 aggregate acceptance. Commit with Sprint #418, this Story-ID and Agent trailer. Heavy/native gates on manager-claimed remote host, no broad <door-host> build.

Manager acceptance at 07:55 UTC: source from run #82 is integrated as a partial. The exact Linux package gate initially hit a meet TempDir cleanup race, then the failing test and full meet package rerun passed. Meet and SDLC foreground conversions are present. `pkg/svcd` and webconsole still retain the old lifecycle; child Story-ID 929f6ffd8818 owns that dependency. This scoped story stays blocked until the webconsole change, complete seeded unit inventory, and native gates are verified. The merge's automatic done status is not acceptance of the full contract.
