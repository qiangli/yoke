---
id: 47eaf561b27c
kind: bug
title: 'atlas census: pkg/outpostagent has no row (TestCensusCoversEveryPackage fails at a3b2556e)'
seq: 3094
status: todo
priority: p2
created: 2026-11-11T00:30:30.206144Z
---

Pre-existing at yoke a3b2556e (llm share --satellite, 8d07c6b4): go test ./pkg/atlas fails only TestCensusCoversEveryPackage. One-line fix: a cmdPkg/libPkg row for pkg/outpostagent in pkg/atlas/packages.go. Found during S604 C2; not repaired there per the root-cause rule.

## 2026-11-13 sweep note

Still red on yoke 998bb871, and also red at the pre-sprint baseline
dc698f7c (so pre-existing). The census now also misses pkg/boundary
(added 4bbb57fb, 11-10, weave #39 "member: add Cloudbox member verbs and
boundary checks"), in addition to pkg/outpostagent here and
pkg/nativepath + pkg/nfthelper (todo b59987279976).
