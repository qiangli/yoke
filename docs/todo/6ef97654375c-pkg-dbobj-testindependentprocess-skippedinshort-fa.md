---
id: 6ef97654375c
kind: bug
title: pkg/dbobj TestIndependentProcess_SkippedInShort fails on main (Linux WSL)
seq: 3077
status: assigned
priority: p3
created: 2026-11-10T03:48:02.949697Z
assignee: s575-codex-manager
sprint: 658
sprint_id: f426cf37-7c11-5d0c-beca-7e405177868d
sprint_title: outpost MCP admin token leaked to daemon.log (fix with the refactor/rebuild)
---

Pre-existing: fails on yoke 1eaaf672 and 6c414c3d in the Windows test host Ubuntu WSL distro (full go test, S602 close). Not repaired in S602. Evidence: dhnt-docs docs/sprint-602-evidence.md.

2026-11-18 authorized separate database repair: official locked-image candidate ec36645f and untouched rc.37 control c9057f1e both reproduce TestIndependentProcess_SkippedInShort with SQLITE_BUSY. rc.37 package evidence used go test -short, so this test was skipped. Database sources are unchanged between control and candidate. Root cause: deferred object transactions read preconditions before obtaining the SQLite writer; a competing writer makes WAL read-to-write upgrade fail immediately instead of waiting within busy_timeout. Historical fix 4895bc9f addressed this on November 2; wholesale reviewed-baseline restoration 22a11d4d removed it November 3, before rc.37. No database-specific rejection was recorded in that restoration.

Restore only the bounded BEGIN IMMEDIATE object mutation transaction handling and deterministic contention/cancellation regressions, preserving revision checks and existing tests. No shell engine, Bashsharp or Coreutils changes.

Verification: official locked Go 1.27.2 image, GOWORK=off, 8 CPUs and package parallelism1: unchanged independent-process test20 passes; deterministic contention/cancellation regressions10 passes; full dbobj/dbstore adapter/dblifecycle/cmds-db suites and vet passed. Final composition with the shared redactor passed the affected database suites and Windows/Darwin/Linux vet. Whole-Yoke crossvet still reports39 errors matching the unchanged canonical baseline; not reported as green.
