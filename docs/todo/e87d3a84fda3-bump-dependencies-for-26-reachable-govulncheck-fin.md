---
id: e87d3a84fda3
kind: security
title: Bump dependencies for 26 reachable govulncheck findings
seq: 2011
status: todo
priority: p2
labels:
    - release
    - security
created: 2026-10-13T04:20:59.820187Z
---

Go 1.27.2 has 0 reachable standard-library findings. Reachable third-party findings: GO-2025-4233, GO-2026-4985, GO-2026-5061, GO-2026-5062, GO-2026-5066, GO-2026-5676, GO-2026-5761, GO-2026-5853, GO-2026-5932, GO-2026-5970, GO-2026-6061, GO-2026-6213, GO-2026-6214, GO-2026-6222, GO-2026-6253, GO-2026-6255, GO-2026-6303, GO-2026-6348, GO-2026-6354, GO-2026-6355, GO-2026-6505, GO-2026-6603, GO-2026-6610, GO-2026-6611, GO-2026-6612, GO-2026-6617. Bump dependencies, publish producer first, refresh consumers, and rerun the Bashy release checker until reachable=0.
