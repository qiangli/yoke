---
id: d91c46d83416
kind: feature
title: S17 Headlamp and act_runner unit seeds with complete host and sandbox parameters
seq: 1898
status: todo
priority: p1
labels:
    - yoke
created: 2026-10-11T09:04:16.17236Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Scoped continuation of original S17 eae40495aac7. In Yoke baseline service registry, seed Headlamp port-forward as a generic process unit and act_runner as generic internal unit capability outpost.actrunner, matching outpost S18 source e87a39a and existing docs/settings.md. Preserve Headlamp mesh name, namespace/target/listen/port, health and install prerequisite; preserve act_runner instance/labels/registration token, host and sandbox lanes, sandbox image and Docker host, vault/env integration, restart/backoff, no fake process. Both use normal service add/show/set/enable/disable/status/verify and no service-name special cases or old config reader. Source only pkg/fleet/baseline/services, adjacent tests and service design docs; do not edit outpost, pkg/chat, pkg/toolcmd or S6 meet. Verify GOWORK=off go test ./pkg/fleet/... ./pkg/service/... plus invocation expansion/round-trip/port and secret tests. Manager owns paired-host live proof. Commit correct Sprint/Story/Story-ID/Agent trailers; Dragon short tests, heavy gates remote.
