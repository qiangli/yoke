---
id: 90064599529f
kind: feature
title: S17 external Zot SeaweedFS Kopia unit seeds with pinned launcher parity
seq: 1892
status: assigned
priority: p1
labels:
    - yoke
created: 2026-10-11T08:23:59.2183Z
weave: 92
assignee: agy-gemini3.8-flash
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Scoped child of original S17 eae40495aac7. In yoke pkg/fleet/baseline/services add generic process-provider service YAML seeds for Zot OCI registry, SeaweedFS S3 gateway, and Kopia backup server. Preserve existing options and defaults from yoke/external/{zot,seaweedfs,kopia} and outpost startup: loopback bind/ports, data dirs, version/digest provisioning, health, mesh names registry/s3/backup, environment/secrets and upgrade behavior. Reuse S15 service schema and S16 generic monitor; no name-specific monitor/outpost branches, no old-format aliases. Add focused parse/validate/ExpandExec and manager runtime tests with local fake services; document any binary CLI mismatch or schema gap rather than inventing flags. Do not edit pkg/fleet/commands.go or pkg/toolcmd (S11 HTTP), pkg/chat/acp, Bashy S19/S20 or outpost S18. Worker verify GOWORK=off go test ./pkg/fleet/... ./pkg/service/...; manager checks Linux live unit start/health/state and S18 exposure. Commit with Sprint #418 and this Story-ID/Agent trailer. Original S17 remains open for act_runner/Headlamp/internal/full inventory.
