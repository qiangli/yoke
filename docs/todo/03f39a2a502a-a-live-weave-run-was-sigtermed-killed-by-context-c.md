---
id: 03f39a2a502a
kind: bug
title: a live weave run was SIGTERMed (killed_by context cancelled, exit 143) while the conductor abandoned a sibling run and ran pkg/weave tests in the same checkout
seq: 36
status: todo
priority: p1
labels:
    - weave
created: 2026-10-09T15:12:04.489732Z
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

S379 2026-10-09: yoke weave run 17 (claude-sonnet5.5, launched 07:58:41 PDT) was killed at 08:10:38 PDT (queue: state killed, exit_code 143, killed_by context cancelled) while working normally. In the same minutes the conductor (a) ran go test -count=1 ./pkg/weave (285 s, finished about 08:10) in the canonical yoke checkout whose weave queue (yoke-d9ca9236) held run 17, and (b) ran weave abandon 14 --yes --disposition superseded in that queue at about 08:10:3x. Other repos' live runs (ycode 3, bashy 12/13) were untouched. Find which one cancels a sibling wrapper's context: the abandon/teardown guarded cleanup (does it signal wrappers of other issues, e.g. by process pattern or queue-wide reconcile?) or pkg/weave tests touching the real ~/.bashy/weave state (tests must use a temp HOME/BASHY_HOME). Red/green test: abandoning run N never signals run M's wrapper; pkg/weave tests never touch the real weave dir.
