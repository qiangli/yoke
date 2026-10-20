---
id: 01a15843-6e96-7b7d-b9e6-517619446464
seq: 6
form: page
type: runbook
title: 'resource watch: install and operate the host resource watcher'
description: How to install, tune and read bashy resource watch — use when a host needs disk/memory/CPU/GPU/network threshold alarms or when auto-registered host resources appear in resource list
tags:
    - resource
    - watch
    - alarms
    - runbook
status: candidate
evidence: S480 story 88518759682b; pkg/resources watch tests; live --once sample on the build host (private sprint record)
source:
    tool: opencode-glm-5.3-v
    episode: ep-2569ab4348efccc9
    parent_episode: ep-6af0d55c01552e6f
created: "2026-10-20T07:42:56Z"
---

`bashy resource watch` samples this host's registered resources and alarms on
threshold transitions (S480). Full design: docs/resource-watch.md.

Install (one scheduled sample per minute):

    bashy schedule add --name resource-watch --every 1m -- bashy resource watch --once --notify

First run auto-registers `<host>`, `<host>.cpu`, `<host>.mem`,
`<host>.disk-*`, `<host>.net-*`, `<host>.gpu-*` into the ordinary resource
registry (idempotent; `resource rm` is undone by the next run — disable with
`resource set NAME --watch enabled=false`).

Tune: defaults < host record < resource record, all via
`resource set NAME --watch k=v` (disk-free-warn=100G, cpu-warn=90,
link-mbps=1000, notify-to=seat, renotify=15m, ...). State persists across
--once runs (hysteresis, 5m sustain windows, re-notify clocks) in
watch-<host>.json under the resources state dir.

Gotchas:
- Sustained metrics (cpu/gpu-util/net-throughput) do not alarm on a single
  spike; the window must cover 5 minutes. Don't expect an alarm from one
  `--once` run.
- darwin reports no GPU utilization/VRAM-usage (no cgo): those metrics are
  skipped there, and interface error counters/link speed are Linux-only
  unless link-mbps is set by hand.
- The disk free floor skips mounts smaller than the floor (boot partitions).
- Remote hosts each run their own watcher; the watcher never holds ssh
  credentials.
