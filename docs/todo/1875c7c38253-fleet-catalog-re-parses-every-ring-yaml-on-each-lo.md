---
id: 1875c7c38253
kind: bug
title: fleet catalog re-parses every ring YAML on each lookup; sprint assign takes 2-3.5 min to pick an agent
seq: 3093
status: todo
priority: p1
created: 2026-11-10T21:05:27.922967Z
---

Measured 2026-11-10 on the dev box (yoke 56edc4f3): `bashy sprint assign --dry-run` alone = 110 s wall / 134 s CPU, nothing launched; three concurrent assigns ~3.5 min each (CPU contention, no lock wait, no sockets). `bashy agent show NAME` = 5.7 s, `bashy agent list` = 10.6 s.

Root cause (sample of a running dry-run: gopkg.in/yaml.v3 parse + regexp/syntax compile dominate): pkg/fleet/catalog.go has no cache. Catalog.Tool(name) calls Tools(true), which rebuilds assetring rows from disk every call; Binding(nick) does three such scans (agent, tool, model); Models/Plans the same. pkg/weave/sprint_seats.go seatPool loops the roster (211 bindings) calling Tool/Binding up to four times per entry, and sprint_band_gate.go builds the pool again: about 2,500 full parses per assign.

Where the time goes (inclusive samples by parser): ParseTool 52%, ParseModel 23%, ParsePlan 16%, ParseAgentFile 9%. One parse reads tools 18 files (baseline only), models 58, plans 5, agents 265 — so the cost is the SAME 18 tool files and 5 plan files re-parsed thousands of times (large CLI contracts, command blocks, usage-signal regexps recompiled per parse via Tool.ValidateUsageSignals), not the agent count.

Fix: memoize parsed rows per Catalog instance (invalidate on Register/write); compile usage-signal regexps once; build the seat pool once per assign. Separate hygiene: 193 local agent clones (one per sprint seat / weave worker, never archived) and 1,521 instance records (1,279 from 2026-10) are never pruned.
