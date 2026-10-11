---
id: 45ec94749298
kind: bug
title: S18 webconsole trusted-identity SSO reads unit service_sso keys
seq: 1893
status: todo
priority: p1
labels:
    - yoke
created: 2026-10-11T08:25:02.91707Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Scoped cross-repo S18 child d7f31dbff83b after outpost removes agent.json bashy_services. yoke/pkg/webconsole/host_auth.go still reads bashy_services[].sso_secret, so trust_cloud_identity apps lose their HMAC key. Replace that reader directly with outpost service_sso[app route] as minted by outpost conf.EnsureServiceSSOSecret; no legacy fallback or migration. Preserve login/elevation and route-specific trust, reject absent/mismatched secret, never expose it in status/logs. Add focused tests using new config shape and delete obsolete old-key tests; confirm outpost-produced key works end to end. Own pkg/webconsole auth and tests only; do not touch pkg/fleet/commands.go or pkg/toolcmd (S11), baseline services (S17 external), Bashy S19/S20, outpost source. Verify GOWORK=off go test ./pkg/webconsole/... and manager Mac/Linux focused gate, then paired-host S18 live gate. Commit with Sprint #418 and this Story-ID/Agent trailer.
