---
id: 6848aa300e5f
kind: bug
title: S557 isolate macOS delegate timing and Linux graph fixture environments
seq: 3049
status: wontfix
priority: p1
labels:
    - superseded
created: 2026-11-01T22:19:00.177881Z
assignee: s557-public-tooling
sprint: 557
sprint_id: f5ccf563-f5d1-5b94-8be4-9388e51ef86d
sprint_title: 'Bashy v1.0.0 preparation: split private/public development and retire qiangli/dhnt'
closed: 2026-11-02T05:32:11.658986Z
---

Remainingdeterministicnative41332670393 macOSfailures: TestDelegateVendorContract/claude/stderr_resets_idle hits150ms idle despite ticks; TestLinuxEngineSecureJoinIsPermissive crossGoListinheritsremote tags causingpkg/api/server allfilesexcluded. Repairdeterministicfixturetiming/environment preservingidle semantics/timeouts and Linuxenginegraphaudit. Coordinatewith lifecycleworker: onlydelegate_guard/testfixture andmplfreefiles, nochatlifecycle. Isolatedworktree, boundedtests, nofullsuites, committrailers,nopush.

Delivery evidence:

- The old heartbeat fixture launched an external sleep process between each
  stderr tick. The retained failure proves a 150ms inactivity interval after
  two ticks, but does not identify a measured scheduling gap. Replace only
  that fixture with one test-binary helper: an immediate shell startup tick,
  then immediate Go stderr output and a 10ms ticker over at least 300ms.
  Keep the original 150ms idle limit and five-second completion assertion.
  Record elapsed heartbeat times separately because successful delegate output
  intentionally excludes stderr. Add a quiet-child negative case with the
  same 150ms idle limit; retain the existing 500ms negative case.
- The Linux graph probe inherited desktop GOFLAGS=-tags=remote, excluding
  Podman's !remote Linux API server. Supply explicit server-side build tags
  to go list while preserving unrelated inherited Go flags. No graph or
  license assertions are removed or skipped.
- Bounded native macOS verification, GOMAXPROCS=2, GOFLAGS=-p=2 plus desktop
  remote/exclude_graphdriver_btrfs/containers_image_openpgp tags, GOWORK=off,
  60-second test deadline: Linux graph and environment guards PASS (0.986s).
  Claude/Codex positive heartbeats and 150ms quiet-child negatives, each twice,
  PASS (eight subtests; chat package 12.971s). Each positive helper emitted
  31 writes over 300.8–302.0ms; maximum recorded inter-write gap 12.01ms.
  These are local bounded measurements, not a claim about arbitrary host
  scheduling stalls. No production timeout or chat lifecycle code changed.
  Full hosted native rerun remains required for integration acceptance.

## Superseded by consolidation — 2026-11-02

Operator requested consolidation of implementation repairs into acceptance owners. Scope, original acceptance, unresolved work and evidence transfer to public umbrella #6, story `bf7624a5c2f2`. This record is closed by decision with label `superseded`; it is NOT a delivery acceptance, test pass, waiver or feature retirement. The parent remains open. Continue corrections under that parent rather than creating another repair story. Original content above is retained unchanged.
