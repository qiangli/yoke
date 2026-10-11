---
id: 2a64539547f2
kind: bug
title: S17 verify external service release assets for Linux
seq: 1903
status: todo
priority: p1
labels:
    - service
    - binmgr
created: 2026-10-11T09:58:41.423714Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Claimed Linux live service gate on Bashy ab3db8d4: zot v2.1.21 fails sha256 mismatch got 8751cc0d... want d2422616...; SeaweedFS 4.48 fails no trustworthy sha256 for linux_amd64.tar.gz. Both units disabled after failure. Scope yoke pkg/binmgr and external/zot, external/seaweedfs and service seed version only as necessary. Investigate actual official release asset, sidecar/checksum semantics and version mapping. Fix reusable verified release resolution or commit independently verified exact-version/platform SHA-256 pins; never disable checksum checks or rely on BASHY_ALLOW_WEAK_CHECKSUM. Include focused regression for each observed failure, at least Linux amd64 plus cross-platform selection. Preserve all current service features and declared unit machinery. Dragon focused short tests only. Commit source and tests in private yoke workspace with Sprint: #418, Story: #1903, Story-ID: 2a64539547f2, Agent: assigned binding; do not push/merge. Manager runs claimed Linux live Zot and SeaweedFS start/health after integration; source work alone is not original S17 acceptance. Send blocker promptly.
