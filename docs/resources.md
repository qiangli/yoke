# `pkg/resources` — live host resource telemetry

The steward can see every run, sprint, and todo on the machine, but not
whether the **machine itself** is the reason they are all slow. This
package supplies that missing axis: a live system-level reading of CPU,
memory, disk, network, and GPU, surfaced as `bashy resource` and
as the board's `resources` panel.

## Surfaces

```
bashy resource system                  # human host table
bashy resource system --json           # bashy-resources-v1 envelope
bashy resource usage                   # host totals + active weave usage by repo
bashy resource usage --by sprint       # repo|sprint|todo|run|agent
bashy resource usage --sprint 177 --json
bashy resource usage --watch --duration 1m
bashy steward dashboard --expand resources  # the panel, inside the steward dashboard
```

`bashy resources` is a hidden compatibility alias. Workspace disk usage is
apparent file bytes from the shared bounded scanner. CPU and RSS are attributed
to a weave workload only through its verified process-root birth identity; GPU
is host-level because the portable collectors cannot attribute it per process.

The host mounts it with `resources.NewCommand()` (cobra). The board wires
itself: `board.DefaultSources()` includes the collector and
`board.DefaultPanels()` includes the panel.

## How the numbers are obtained

Pure Go, no cgo, no shelling out — the repo's two hard rules apply here as
much as to the userland. Every figure is read from an OS interface
directly:

| | Linux | darwin | Windows |
|---|---|---|---|
| CPU | `/proc/stat` (aggregate + per-core ticks) | `vm.loadavg` sysctl | `NtQuerySystemInformation` class 8 (per-core ticks) |
| load avg | `/proc/loadavg` | `vm.loadavg` | — (no such concept) |
| memory | `/proc/meminfo` | `hw.memsize` + `kern.memorystatus_level` + `vm.swapusage` | `GlobalMemoryStatusEx` |
| disk | `/proc/self/mountinfo` + `statfs` | `getfsstat` | `GetLogicalDrives` + `GetDiskFreeSpaceEx` |
| disk IO | `/proc/diskstats` | — | — |
| network | `/proc/net/dev` | `NET_RT_IFLIST2` routing dump | `GetIfTable` |
| GPU | `/proc/driver/nvidia/*`, `/sys/class/drm/card*` | `hw.optional.arm64` → the Apple silicon integrated GPU (unified VRAM) | display-class registry key (`DriverDesc`, `HardwareInformation.qwMemorySize`) |

Deliberate gaps, stated rather than papered over:

- **darwin has no CPU tick sysctl.** `kern.cp_time` does not exist on
  macOS; per-core utilization lives behind `host_processor_info`, a mach
  trap that needs cgo. The darwin reading is therefore load-average
  derived (runnable threads per core, clamped to 100) and says so in
  `cpu.source`; per-core is omitted rather than fabricated.
- **darwin GPU detail needs IOKit.** Apple silicon is reported (its VRAM
  *is* `hw.memsize`, `vram_kind: "unified"`); an Intel Mac's discrete GPU
  is not enumerated, and no utilization figure is available on either.
  `system_profiler` would answer this — and is exactly the shell-out the
  repo forbids.
- **Windows disk IO** would need the PDH performance library; omitted.
- A GPU-less host is normal, not an error: `gpus` is empty and the table
  prints `none detected`.

Anything a platform cannot supply lands in `warnings[]`. A subsystem
failing never fails the whole reading.

## Sample-and-diff

Utilization is a rate, so `Collect` takes two counter samples
`--interval` apart (default 300ms) and reports the delta; the command
blocks for that interval. Two degradations, both explicit:

- `--interval 0` skips the second sample. CPU then reports the
  **since-boot** ratio (`cpu.source: "ticks-boot"`) and byte rates are
  omitted rather than invented. This is the path the board panel uses, so
  refreshing the board never pays a sampling delay.
- A counter that moves backwards (a container boundary, a driver reload)
  yields 0, not a garbage spike.

Every envelope carries an explicit `at` timestamp and `sample_seconds`.

## Pressure

CPU and memory use one three-level vocabulary — `normal` (<75%),
`elevated` (75–90%), `critical` (≥90%). Disk escalates earlier
(`elevated` at 85%, `critical` at 95%) because a full disk is a hard stop,
not a slowdown.

## Mount deduplication

One APFS container backs `/`, `/System/Volumes/Data`, `/System/Volumes/
Preboot`, and five more, and every one reports the container's capacity;
Linux bind mounts and btrfs subvolumes do the same. Listing them all makes
one 460GB disk look like eight and buries the number that matters, so
mounts are collapsed per pool (by device generally, by APFS container on
darwin), keeping the shortest mount path.

## Envelope

`bashy-resources-v1`, versioned like the other bashy envelopes:

```json
{
  "schema_version": "bashy-resources-v1",
  "at": "2026-07-21T10:04:25Z",
  "host": "dragon", "os": "darwin", "arch": "arm64",
  "sample_seconds": 0.301,
  "cpu":     {"logical_cores": 12, "usage_percent": 57.4, "pressure": "normal", "source": "loadavg", "load_average": [7.8, 7.5, 7.1]},
  "memory":  {"total_bytes": 25769803776, "used_bytes": 7180000000, "used_percent": 27, "pressure": "normal"},
  "disks":   [{"mount": "/", "fstype": "apfs", "total_bytes": 494384795648, "used_percent": 97.7}],
  "network": [{"name": "en0", "rx_bytes": 497000000, "rx_bytes_per_sec": 533913.6}],
  "gpus":    [{"vendor": "Apple", "name": "Apple M4 Pro GPU", "vram_bytes": 25769803776, "vram_kind": "unified"}]
}
```

## Binary hygiene

`pkg/resources` depends only on the stdlib, cobra, `golang.org/x/sys`, and
(darwin only) `golang.org/x/net/route` — the last purely to fetch the raw
`NET_RT_IFLIST2` bytes, which this package parses itself. It is imported
by `pkg/board` and by the host front door; `cmd/coreutils` links neither,
verified by `go list -deps ./cmd/coreutils`.
