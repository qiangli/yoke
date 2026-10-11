---
id: eae40495aac7
kind: refactor
title: S17 Reimplement all supported built-in daemons as units; delete the four duplicated start/status/stop lifecycles; add llm and events units
seq: 1886
status: todo
priority: p1
labels:
    - yoke
    - coreutils
    - bashy
created: 2026-10-11T05:07:47.965699Z
weave: 104
assignee: claude-opus5.5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Design docs/bashy-service-design.md: inetd/systemd split - bashy service owns unit definitions and per-unit verbs; bashy supervisord (a bashy command, like inetd) is the service monitor; outpost supervisord (init, like systemd) starts and watches only the outpost daemon and bashy supervisord. Today lifecycle code is duplicated: yoke/pkg/svcd (only webconsole uses it), yoke/pkg/meet/service.go, yoke/pkg/sdlc/service.go, coreutils/pkg/schedule/service.go, plus loom own start/status/stop; outpost hardcodes --root-url/--port for loom and --port/--bind for apps. Each daemon exposes a foreground serve with /healthz and becomes a built-in unit; delete the legacy X service start|status|stop subcommands and their old configuration paths; reimplement all supported behavior directly through bashy service units, with no generic migration or transitional aliases; svcd detached pidfile model retires; bashy llm serve gets its unit (no service wrapper today); loom/apps special args move into their unit exec. Acceptance: bashy service status lists all seven built-ins active under bashy supervisord; old service commands/config and duplicated lifecycles are removed; no package other than the monitor writes a service pidfile; bashy make test and make test-bash 86/86 unchanged.

Operator coverage requirement 2026-10-10 PDT: retain every currently supported service and feature in the new design. Seven initial units are not an exhaustive gate. Account for apps/meet/loom/SDLC/schedule, LLM and DAG workloads, Zot/SeaweedFS/Kopia, act_runner host+sandbox lanes, Headlamp port-forward, custom programs/services and retained in-process outpost surfaces. Preserve supported parameters, provisioning/version pins, environment/secrets, login/elevation/SSO, tunnel/mesh names, controls/status and upgrade behavior. Use the complete service/feature inventory in docs/sprint-418-master-execution-plan.md and shared candidate evidence; every entry needs its new unit or retained owner and existing test/live-smoke evidence. Extend the schema as needed; remove old formats directly with no backward compatibility or migration.

Operator rod-not-fish requirement 2026-10-10 PDT: implement reusable service declaration/configuration/enablement machinery shared by built-in, internal and external services. Built-ins are seeds, not service-name branches. Internal capabilities retain their subsystem behind a generic runtime provider and use the same registry, desired state, health/status/events and exposure contracts; no separate internal feature-flag path or fake child process. Acceptance includes seeded built-in + internal capability + fresh custom external declaration using the same service verbs, plus an operator override, without per-service supervisor/toggle/exposure code. Agree the S15/S16/S18 provider contract first; preserve complete existing feature coverage and no legacy compatibility/migration.

Bounded Yoke continuation 2026-10-11: Outpost S18 submitted `e3788fb` registering in-process capability IDs `outpost.files`, `outpost.podman`, `outpost.sandbox`, `outpost.ollama`, `outpost.ollama-pool`, `outpost.warm`, `outpost.shard`, `outpost.lan-inference`, `outpost.otel`, `outpost.ycode` and existing `outpost.actrunner`. Only actrunner has a Yoke seed today. Implement the missing embedded baseline service YAML definitions in `pkg/fleet/baseline/services/` with exact capability IDs, settings/defaults, dependencies, desired state, ports and exposure supported by existing `pkg/fleet` schema. Use Outpost worker documentation `docs/settings.md` and current code for option semantics; do not add generic schema or per-service runtime branches. Add a focused Yoke baseline test that checks these seeds load, match IDs/owner/provider, and retain essential settings and safety defaults. Verify `GOWORK=off go test -short ./pkg/fleet/... ./pkg/service/...`; no broad Dragon build. Commit only in private Yoke; manager independently reviews and tests, integrates Outpost and runs paired live gates. This is a source slice of the original S17, so submit the run with its gaps stated and leave S17 open until all original acceptance, including seven foreground built-ins and complete retained service inventory, passes.

Manager integration 2026-10-11 10:47 UTC: internal outpost seed source `b18612f` merged as Yoke `1d6388b`; independent focused `GOWORK=off go test -short -run 'TestOutpostCapabilitySeeds|TestOllamaPoolRequiresTheProxyUnit|TestLANInferenceDeclaresItsLANPort' ./pkg/fleet` and `GOWORK=off go test -short ./pkg/service/...` passed on the merged tree. Private workspace broad fleet gate lacked test ring models, so it is not a broad PASS. The original story remains open for seven foreground built-ins, inventory and paired live checks.
