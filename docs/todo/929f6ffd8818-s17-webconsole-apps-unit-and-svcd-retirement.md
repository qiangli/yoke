---
id: 929f6ffd8818
kind: refactor
title: S17 webconsole apps unit and svcd retirement
seq: 1888
status: done
priority: p1
labels:
    - yoke
created: 2026-10-11T07:47:35.771135Z
weave: 85
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T08:15:11.13946Z
resolution: fixed
closed_by: codex-gpt6-sol
---

Scoped child of Sprint 418 S17 Story-ID eae40495aac7. Own yoke/pkg/webconsole/service.go and service_profile.go, pkg/svcd, related tests, and the apps seeded unit only. The old webconsole apps lifecycle still consumes pkg/svcd detached pidfiles and profile config. Reimplement apps serve/status/start/stop via the generic service registry and Bashy monitor contract; preserve app pairing, root URL, port/bind, login/elevation/SSO, tunnel routing and status behavior. Retire pkg/svcd and its pidfile lifecycle after all callers are replaced; do not add migration aliases or an apps-name branch in monitor/exposure. Meet/SDLC conversion is in separate S17 child; do not edit those files, nor Bashy S16 or outpost S18. Verify GOWORK=off go test ./pkg/webconsole/... ./pkg/svcd/... ./pkg/fleet/... ./pkg/service/... and process-level apps foreground health/status with a seeded unit; manager owns remote native and final service inventory gates. Commit with Sprint #418, this Story-ID and Agent trailer. Dragon focused short tests only.
