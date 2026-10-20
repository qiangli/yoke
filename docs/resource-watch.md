# resource watch — thresholds, alarms and the host watcher

`bashy resource watch` samples this host's registered resources on an
interval and alarms when one crosses a threshold. It exists because a build
host filled its disk twice in one night while three sprints ran in parallel,
and nobody noticed until builds failed (S480).

Design rules, from the operator spec:

- **One inventory.** The watcher registers the host's cpu, memory
  (including swap), each mounted disk, network interfaces and GPUs into the
  SAME registry `bashy resource add/list/show/set` reads — as `name`-kind
  records named `<host>.cpu`, `<host>.mem`, `<host>.disk-<mount>`,
  `<host>.net-<iface>`, `<host>.gpu-<name>`, plus one `<host>` record that
  carries the per-host defaults. No parallel inventory exists.
- **Cheap.** One short sample per interval (`resources system`'s single
  counter sample, `Interval 0`). Rates (network throughput) are computed by
  diffing cumulative counters between runs — the watcher never sleeps for a
  measurement window and never walks a tree.
- **Transitions only.** A line is printed per transition
  (normal → warning → critical → normal, with a CLEAR notice), hysteresis
  keeps a value hovering at a threshold from flapping, and a still-alarming
  resource is re-notified at a bounded interval.

## Defaults (S480 operator spec)

| Metric | Warning | Critical | Sustain |
|---|---|---|---|
| Disk used | 85% | 95% | — |
| Disk free floor | 20 GiB | 10 GiB | — |
| Memory used | 85% | 95% | — |
| Swap used | 50% | 80% | — |
| CPU (loadavg/cores on darwin, ticks elsewhere) | 85% | 95% | 5 min |
| GPU utilization | 90% | 98% | 5 min |
| GPU memory | 90% | 97% | — |
| Network throughput (of known link speed) | 80% | 95% | 5 min |
| Interface errors/drops (of packets) | 1% | 5%* | — |
| Link down (was up) | — | critical | immediate |

Disk alarms are whichever trips first: used percent or the absolute free
floor. Mounts smaller than the floor itself never trip the byte floor (the
same rule `resource system --min-free` applies), so firmware and boot
partitions stay quiet.

\* The spec fixed the 1% warning level but left the critical level open;
5% is the chosen default. Recorded here per the spec's "adjust only with a
recorded reason" rule — no other value was invented.

Sustained metrics (CPU, GPU utilization, network throughput) only trip once
a window covering the sustain duration holds every point at or above the
threshold; the window itself is their anti-flap, so the 3-point hysteresis
is not applied on top of it (that would double every window). Immediate
metrics use the 3-point hysteresis. A re-notify fires every 30 minutes
while a resource is still alarming.

A metric's FIRST evaluated sample alarms at once when it is already beyond
a threshold (unknown → warning/critical commits without waiting out the
hysteresis) — a disk that was already full when the watcher first saw it
must be heard on that sample, not two scheduler ticks later. The rule
never bypasses a sustain window (CPU, GPU utilization, throughput still
wait out their 5 minutes), and every transition after the first level is
set keeps the full 3-point hysteresis against flapping.

Platform honesty (unchanged from `resources system`): a metric a platform
cannot supply purely is omitted, never approximated. darwin reports no GPU
utilization or VRAM-usage figures (IOKit needs cgo), so those metrics
evaluate only where the numbers exist; interface error counters come from
Linux's `/proc/net/dev`, link speed from `/sys/class/net/*/speed` or the
`link-mbps` override; darwin supplies byte counters and link state only.

## Overrides

Layering: **defaults < host record < resource record**, applied with the
ordinary registry verbs:

```
# the host record sets the operator's free-space floor (e.g. 100 GiB)
bashy resource set host-a.example --watch disk-free-warn=100G

# one disk gets tighter thresholds than the host
bashy resource set host-a.example.disk-data --watch disk-warn-used=80

# a known link speed unlocks the throughput metric on an interface
bashy resource set host-a.example.net-en0 --watch link-mbps=1000

# stop watching one resource entirely
bashy resource set host-a.example.net-awdl0 --watch enabled=false
```

Keys: `disk-warn-used`, `disk-crit-used`, `disk-free-warn`, `disk-free-crit`
(byte sizes like `100G`), `mem-warn`, `mem-crit`, `swap-warn`, `swap-crit`,
`cpu-warn`, `cpu-crit`, `cpu-sustain`, `gpu-warn`, `gpu-crit`, `gpu-sustain`,
`gpu-mem-warn`, `gpu-mem-crit`, `net-warn`, `net-crit`, `net-sustain`,
`net-err-warn`, `net-err-crit`, `link-mbps`, `hysteresis`, `renotify`,
`notify-to`, `enabled`. Drop one with `--rm-watch KEY`.

Removing an auto-registered record with `resource rm` is undone by the next
watch run — the sweep is desired-state. Disable watching instead:
`--watch enabled=false`.

## Alarms

Console (stderr), one line per transition:

```
ALARM host=host-a.example resource=host-a.example.disk-data metric=disk-used level=warning value=86% threshold=85% at=2026-10-20T01:02:03Z
CLEAR host=host-a.example resource=host-a.example.disk-data metric=disk-used value=70% at=2026-10-20T01:40:03Z
```

With `--json`, each event is also emitted as NDJSON on stdout
(`bashy-resource-watch-v1`: `type` alarm|clear|renotify, `host`, `resource`,
`metric`, `from`/`to`/`level`, `value`, `unit`, `threshold`, `detail`,
`at`, `renotify`).

`--notify` additionally delivers each event through the same board path
`bashy mb send` uses, to `--recipient` (default: `$BASHY_RESOURCE_WATCH_TO`,
else `operator`), or to the host record's `notify-to` override. Delivery
failures warn on stderr and never stop the watcher.

## Install (one-liner)

As a scheduled job, one sample per minute:

```
bashy schedule add --name resource-watch --every 1m -- bashy resource watch --once --notify
```

State (hysteresis counters, sustained windows, re-notify clocks, last
network counters) persists in `watch-<host>.json` under the bashy resources
state dir (`$BASHY_HOME/resources`, override with `--state-dir`), so
`--once` runs keep their memory across scheduler invocations. With a 1m
cadence the 5-minute sustain windows carry five points.

Verify the install:

```
bashy resource watch --once        # one sample, registry output on stderr
bashy resource list                # the auto-registered records
bashy resource watch --once --json # NDJSON events on stdout when any fire
```

## Remote hosts

Each host runs its own watcher and reports through the inbox: install the
same one-liner on the host, set its host record's floor
(`resource set <host> --watch disk-free-warn=100G`), and alarms arrive
addressed from that host with `host=` naming it. The watcher never holds or
asks for ssh credentials; optionally sampling a remote host through an
existing scope (@host, sphere) is a follow-up, not part of this install.

## Where the code lives

- `pkg/resources/watch.go` — levels, thresholds, defaults, override vocabulary
- `pkg/resources/watch_eval.go` — metric extraction from a `System` sample
- `pkg/resources/watch_state.go` — hysteresis/transition/re-notify state machine, state file
- `pkg/resources/watch_registry.go` — auto-registration and config resolution
- `pkg/resources/watch_cmd.go` — the `resource watch` front door and inbox delivery
- `cmds/resources/resources.go` — the same engine as `resources watch`
