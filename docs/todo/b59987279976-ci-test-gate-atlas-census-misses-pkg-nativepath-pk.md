---
id: b59987279976
kind: bug
title: 'ci-test-gate: atlas census misses pkg/nativepath + pkg/nfthelper; policy/coord attach test flaky'
seq: 3063
status: todo
priority: p2
labels:
    - ci
created: 2026-11-07T09:22:58.966768Z
---

Pre-existing on yoke origin/main 1b05a5ad (found by S584 final gate, not caused by it). (1) pkg/atlas TestCensusCoversEveryPackage: pkg/nativepath (fdbd69bf, 2026-10-30) and pkg/nfthelper (d820b289) have no entry in pkg/atlas/packages.go; add cmdPkg/libPkg/supPkg/Unwired classification. Not in test/known-failures.txt, so scripts/ci-test-gate.sh reports it as NEW on every run. (2) pkg/policy/coord TestAttachedClaimDiesWithParentAndDoesNotLeakFD: FLAKY (failed in suite, passed isolated): attach_process_unix_test.go:62 strconv.Atoi parsing "".
