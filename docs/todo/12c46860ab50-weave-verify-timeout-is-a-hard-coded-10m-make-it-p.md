---
id: 12c46860ab50
kind: bug
title: 'weave: verify timeout is a hard-coded 10m; make it per-project and report per-line gate status'
seq: 3105
status: todo
priority: p2
labels:
    - weave
    - gate
created: 2026-11-12T18:16:30.266549Z
---

Found 2026-11-12 (Sprint 499, bashy run #102): weave's verify timeout is hard-coded to 10m (pkg/weave/weave_impl.go weaveRunVerify, context.WithTimeout(10*time.Minute)). bashy's configured gate (.bashy/gate: go build ./...; go test ./internal/... ./tools/...; env make test-bash) needs more than that on the dev box.
- Measured at 5d86502f: build 26 s, go test 406 s (16/16 ok; internal/agentos 298 s), then test-bash 86/86. Total over 10m.
- So `weave pull` and `weave salvage` report suite-gate-failed on a green gate (twice in a row), and the conductor had to merge by hand.
- Classification: tooling/venue limit (weave), not a product defect in bashy. The output trim (2000 chars) also hides which gate lines passed.

Fix:
- Make the verify timeout configurable per project (e.g. a `# timeout: 30m` directive in .bashy/gate, or WEAVE_VERIFY_TIMEOUT), with 10m staying the default.
- Report per-line gate status (line, exit, duration) in the result, so a timeout names the step that ran over.

Tests:
- the directive is honoured
- the default is unchanged
- a timed-out gate names the running line
