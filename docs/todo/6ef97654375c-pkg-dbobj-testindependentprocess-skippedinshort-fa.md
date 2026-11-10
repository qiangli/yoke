---
id: 6ef97654375c
kind: bug
title: pkg/dbobj TestIndependentProcess_SkippedInShort fails on main (Linux WSL)
seq: 3077
status: todo
priority: p3
created: 2026-11-10T03:48:02.949697Z
---

Pre-existing: fails on yoke 1eaaf672 and 6c414c3d in the Windows test host's Ubuntu WSL distro (full go test, S602 close). Not repaired in S602. Evidence: dhnt-docs docs/sprint-602-evidence.md.
