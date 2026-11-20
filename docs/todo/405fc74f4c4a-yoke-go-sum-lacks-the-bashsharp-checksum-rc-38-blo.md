---
id: 405fc74f4c4a
kind: bug
title: yoke go.sum lacks the bashsharp checksum (rc.38 blocker)
seq: 3143
status: todo
priority: p1
created: 2026-11-20T16:40:28.872959Z
---

rc.38 gate on elephant (2026-11-20): yoke 79fe8f90 (S575, 11-17) added require github.com/bashsharp/bashsharp v0.0.0-20261118040926-59ce327a5e01 without a go.sum line; cmds/all, pkg/ollm setup failed and pkg/mplfree TestLinuxEngineSecureJoinIsPermissive failed on its go list. Build-metadata defect; fix = add the two checksum lines (go mod download of that exact version, no tidy).
