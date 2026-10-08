---
id: 7048a50eb180
kind: bug
title: codex steer sessions run their shell commands in the shared codex app-server daemon, which drops foreman's injected identity (BASHY_AGENT_ID, BASHY_PRINCIPAL, BASHY_FLEET_TOKEN, ...)
seq: 21
status: todo
priority: p0
labels:
    - fleet
created: 2026-09-30T11:57:43.252951Z
sprint: 321
sprint_id: 772f4aa9-d0ce-568e-8432-e5b52fc021e5
sprint_title: 'Agent families and UUID instances: reliable inbox, family ratings and scoped deputies'
---

Found by the steward 2026-09-30, blocking Sprint #319 (codex-gpt6-sol conductor recorded 'conductor (bypass)' on checkpoint/track/assign and stopped manager actions). The foreman-launched codex TUI (pinned 0.157.1, steer_exec 'codex --model {model}') has BASHY_AGENT_ID/BINDING/PRINCIPAL/FLEET_TOKEN in its process env, but its shells only had BASHY_AGENTS_PATH/BASHY_MODELS_PATH: commands execute in the shared managed app-server daemon (~/.codex/packages/app-server-daemon/releases/0.159.2, pid-update-loop since 2026-09-26, feature daemon_auto_start=true), whose env is the login profile's. Also runs tools in 0.159.2 despite the 0.157.1 pin. codex exec is unaffected (in-process). Fix: steer_exec 'codex --no-daemon --model {model}' (baseline + host override). Red/green: baseline test asserts the codex steer launch carries --no-daemon.
