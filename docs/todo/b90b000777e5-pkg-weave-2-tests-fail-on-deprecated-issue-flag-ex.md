---
id: b90b000777e5
kind: bug
title: 'pkg/weave: 2 tests fail on deprecated --issue flag (exit 1)'
seq: 2896
status: todo
priority: p3
created: 2026-10-29T08:00:41.901395Z
---

Pre-existing on yoke fa623dcd (seen 2026-10-29 S525 A7 review): TestDivergedBaseStillCountsAheadViaBaseSHA (weave_diverged_base_test.go:40) and TestWeavePullRefusesVerdictWhenBaseMovesDuringReview (weave_pair_review_test.go:146) call 'weave start --issue N'; the deprecated flag now exits 1. Fix: switch the tests to --run. Verify: GOWORK=off go test ./pkg/weave -run 'TestDivergedBase|TestWeavePullRefusesVerdict'.
