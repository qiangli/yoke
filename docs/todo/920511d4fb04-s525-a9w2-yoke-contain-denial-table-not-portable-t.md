---
id: 920511d4fb04
kind: bug
title: 'S525.A9w2 yoke: contain denial table not portable (tests exec POSIX cat), fails/skips on native Windows'
seq: 2926
status: done
priority: p0
labels:
    - windows
created: 2026-10-29T17:49:03.063457Z
weave: 711
assignee: claude-opus5.5
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T17:55:16.352988Z
resolution: fixed
closed_by: claude-opus5.5-i
---

Sprint 525, goal a9 (parent eb1be776). Rework of S525.A9w df339248 (merged yoke d9231209), found by the manager's NATIVE Windows run (elephant, bashy-managed Go, `bashy go test ./pkg/procguard ./pkg/shape/contain` at d9231209).
procguard: PASS on Windows (TestGuardNormalRun, TestJobObjectContainsSessionEscapes, TestGrandchildDiesWithParentJob).
pkg/shape/contain TestDenialTable FAILS on native Windows. The real denial cases pass (env_scrubbed, effect_cap_refusal, unsupported_level_is_typed_refusal); two subtests fail only because they exec `cat`, a POSIX binary absent on Windows:
  declared_effects_stand_in_for_the_atlas_lookup: contain: exec: "cat": executable file not found in %PATH%
  process_level_runs_with_scrubbed_env: same
and default_level_is_bashy_guards_no_sandbox_needed SKIPs ("no cat"). A9w's acceptance required the denial table to pass on a real Windows host.
Fix: make these tests OS-independent. Use the go test binary itself as the helper process (the TestHelperProcess pattern: re-exec os.Args[0] with an env switch, printing env / reading stdin) instead of external POSIX tools; no skips for Windows. Production code is unchanged unless a real defect shows up.
Acceptance: go test ./pkg/shape/contain green on darwin AND on a native Windows host (the manager runs it there) with ZERO skips on Windows; GOOS=windows vet. Sprint/Story/Story-ID trailers; no hostnames.
