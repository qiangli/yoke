---
id: d1d045dd536f
kind: feature
title: 'S15 service as a resource kind: unit YAML in the registry rings with CRUD and seeded built-in units'
seq: 1884
status: done
priority: p1
labels:
    - yoke
    - bashy
created: 2026-10-11T05:07:43.3467Z
weave: 72
assignee: claude-opus5.5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T06:55:21.342036Z
resolution: fixed
closed_by: claude-opus5.5-t
---

Design docs/bashy-service-design.md: inetd/systemd split - bashy service owns unit definitions and per-unit verbs; bashy supervisord (a bashy command, like inetd) is the service monitor; outpost supervisord (init, like systemd) starts and watches only the outpost daemon and bashy supervisord. Add service to the extension kinds (resource/command/service/model/tool/agent/skill); the S1 matrix covers seven kinds. bashy service add -f unit.yaml, show --yaml, set, rm, list --json; units live in the registry rings (embedded built-ins, shared, local), built-ins seeded as compiled-in definitions never copied: apps (console 22749), meet (8637), loom, sdlc, schedule, llm (door 24556, new), events (S3, new). Port conflicts against other units are refused at add/enable. Acceptance: show --yaml piped to add -f - is identity for a built-in and a custom unit; a conflicting port is refused with the holder named; every verb is an MCP tool via bashy mcp.

Operator coverage requirement 2026-10-10 PDT: retain every currently supported service and feature in the new design. Seven initial units are not an exhaustive gate. Account for apps/meet/loom/SDLC/schedule, LLM and DAG workloads, Zot/SeaweedFS/Kopia, act_runner host+sandbox lanes, Headlamp port-forward, custom programs/services and retained in-process outpost surfaces. Preserve supported parameters, provisioning/version pins, environment/secrets, login/elevation/SSO, tunnel/mesh names, controls/status and upgrade behavior. Use the complete service/feature inventory in docs/sprint-418-master-execution-plan.md and shared candidate evidence; every entry needs its new unit or retained owner and existing test/live-smoke evidence. Extend the schema as needed; remove old formats directly with no backward compatibility or migration.

Operator rod-not-fish requirement 2026-10-10 PDT: implement reusable service declaration/configuration/enablement machinery shared by built-in, internal and external services. Built-ins are seeds, not service-name branches. Internal capabilities retain their subsystem behind a generic runtime provider and use the same registry, desired state, health/status/events and exposure contracts; no separate internal feature-flag path or fake child process. Acceptance includes seeded built-in + internal capability + fresh custom external declaration using the same service verbs, plus an operator override, without per-service supervisor/toggle/exposure code. Agree the S15/S16/S18 provider contract first; preserve complete existing feature coverage and no legacy compatibility/migration.
