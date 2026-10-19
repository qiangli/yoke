# Sprint 467 story #2383 (43f01cb71db9) — `weave abandon --force` teardown

Host: dev-host (macOS). Caps: `GOFLAGS=-p=2 GOMAXPROCS=4 BASHY_INSTANCE_CAP=32`.
Filtered tests only; the manager runs whole `pkg/weave` + `scripts/crossvet.sh` remotely.

## Root cause (read-only inspection of live leftover rows, nothing touched)

Leftover abandoned yoke workspaces were porcelain-clean with HEAD preserved at
`refs/salvage/abandoned-N`, but held commits outside HEAD: run 454 a
`refs/stash` (9 commits in neither base nor salvage), run 466 a side branch
`refs/heads/wip-s453-context-pre-squash` (3 commits). `--force` preserved only
HEAD, so `weaveUnsettledLocalRefs` (rightly) refused settlement,
`weavePruneOwnedRun` returned nothing, and abandon printed the generic
"teardown deferred (wrapper still winding down, or settlement unproven)".

## Initial probe (disclosed — NOT a reproducer)

The first fixture modeled only a HEAD-salvaged clean terminal run (no stash,
no side branch). It PASSED on the unmodified code: `--force` tore that shape
down. So the defect is not in the HEAD path. The RED below comes from the
side-work fixture that matches the leftover rows.

## Fix

- `--force` also imports every unsettled local workspace ref into the user repo
  as `refs/salvage/abandoned-<id>-<ref>` (never-move naming shared with the HEAD
  preserve), and settlement accepts commits reachable from those refs.
- When teardown still refuses, abandon names the exact reason (eligibility error,
  e.g. unverified child, or the refs holding unpreserved commits).

## Commands

```
GOFLAGS=-p=2 GOMAXPROCS=4 BASHY_INSTANCE_CAP=32 go test ./pkg/weave -run 'TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork|TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused|TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused|TestWeaveAbandonForceSideRefCollisionNeverOverwritesSalvage' -count=1 -v
go test ./pkg/weave -run 'Abandon|PruneOwnedRun|Salvage|Settled|Unsettled|Disposition|StoryEnd|Preserv|Prune' -count=1
```

## RED (source fix reverted, the original 3 tests; raw /tmp/red-2383.txt) — exit 1

```
=== RUN   TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork
    weave_abandon_force_teardown_test.go:53: abandon output:
        weave abandon: run #1 abandoned as superseded (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace /var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork1034844821/001/.bashy/weave/002-2365679c/workspaces/issue-1: teardown deferred (wrapper still winding down, or settlement unproven) — `weave prune` reclaims it
    weave_abandon_force_teardown_test.go:58: settled workspace left alone:
        weave abandon: run #1 abandoned as superseded (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace /var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork1034844821/001/.bashy/weave/002-2365679c/workspaces/issue-1: teardown deferred (wrapper still winding down, or settlement unproven) — `weave prune` reclaims it
--- FAIL: TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork (1.54s)
=== RUN   TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused
    weave_abandon_force_teardown_test.go:80: abandon output:
        weave abandon: run #1 abandoned as rejected (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace /var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused931952567/001/.bashy/weave/002-ee26a351/workspaces/issue-1: teardown deferred (wrapper still winding down, or settlement unproven) — `weave prune` reclaims it
    weave_abandon_force_teardown_test.go:86: refusal does not name refs/stash:
        weave abandon: run #1 abandoned as rejected (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace /var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused931952567/001/.bashy/weave/002-ee26a351/workspaces/issue-1: teardown deferred (wrapper still winding down, or settlement unproven) — `weave prune` reclaims it
--- FAIL: TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused (1.45s)
=== RUN   TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused
    weave_abandon_force_teardown_test.go:103: abandon output:
        weave abandon: run #1 abandoned as rejected (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace /var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused2830369224/001/.bashy/weave/002-6c7869ef/workspaces/issue-1: teardown deferred (wrapper still winding down, or settlement unproven) — `weave prune` reclaims it
    weave_abandon_force_teardown_test.go:111: deferral does not say why:
        weave abandon: run #1 abandoned as rejected (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace /var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused2830369224/001/.bashy/weave/002-6c7869ef/workspaces/issue-1: teardown deferred (wrapper still winding down, or settlement unproven) — `weave prune` reclaims it
--- FAIL: TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused (0.87s)
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	4.974s
FAIL
```

## GREEN (with fix, original 3 tests; raw /tmp/green-2383.txt) — exit 0

```
=== RUN   TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork
    weave_abandon_force_teardown_test.go:53: abandon output:
        weave abandon: run #1 abandoned as superseded (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: side work preserved at refs/salvage/abandoned-1-heads-wip-pre-squash
        weave abandon: side work preserved at refs/salvage/abandoned-1-stash
--- PASS: TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork (2.26s)
=== RUN   TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused
    weave_abandon_force_teardown_test.go:80: abandon output:
        weave abandon: run #1 abandoned as rejected (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace /var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused4182058025/001/.bashy/weave/002-8a6163df/workspaces/issue-1: teardown refused: workspace ref(s) hold commits in neither base nor a salvage ref: refs/heads/wip-pre-squash, refs/stash — `weave prune` reclaims it once resolved
--- PASS: TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused (1.55s)
=== RUN   TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused
    weave_abandon_force_teardown_test.go:103: abandon output:
        weave abandon: run #1 abandoned as rejected (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace /var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused2658189970/001/.bashy/weave/002-3468585b/workspaces/issue-1: teardown refused: run child termination remains unverified — `weave prune` reclaims it once resolved
--- PASS: TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused (0.75s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	5.465s
```

## Neighbor regression filter — exit 0

```
ok  	github.com/qiangli/yoke/pkg/weave	76.418s
```

## Collision/retry test — first run FAILED on a test bug (raw /tmp/collision-testbug-2383.txt)

The source behaved correctly, but the assertion used `for-each-ref` patterns without `*`, which only match exact refs. Excerpt:

```
=== RUN   TestWeaveAbandonForceSideRefCollisionNeverOverwritesSalvage
    weave_abandon_force_teardown_test.go:135: abandon output:
        weave abandon: run #1 abandoned as superseded (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: side work preserved at refs/salvage/abandoned-1-heads-wip-pre-squash-561307d5a2ce9927432d81cc70cd3e19e56d4e29
        weave abandon: side work preserved at refs/salvage/abandoned-1-heads-x-y
        weave abandon: side work preserved at refs/salvage/abandoned-1-heads-x-y-437313c29071eacc2fd68e580f3b6f31e15d1170
        weave abandon: side work preserved at refs/salvage/abandoned-1-stash
--- FAIL: TestWeaveAbandonForceSideRefCollisionNeverOverwritesSalvage (2.52s)
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	13.459s
FAIL
exit=1
```

## FINAL GREEN (fix + 4 tests incl. collision/retry; raw /tmp/green-final-2383.txt) — exit 0

The collision test pre-creates `refs/salvage/abandoned-1-heads-wip-pre-squash` at an older tip, plus two branches (`x/y` and `x-y`) that flatten to the same name. Every existing salvage ref is unmoved; the colliding pair lands at `-heads-x-y` and `-heads-x-y-<sha>` at their exact tips; the moved wip tip lands at `-<sha>`; and all tips stay reachable after removal.

```
=== RUN   TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork
    weave_abandon_force_teardown_test.go:53: abandon output:
        weave abandon: run #1 abandoned as superseded (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: side work preserved at refs/salvage/abandoned-1-heads-wip-pre-squash
        weave abandon: side work preserved at refs/salvage/abandoned-1-stash
--- PASS: TestWeaveAbandonForceTearsDownPreviouslyAbandonedRunWithSideWork (1.86s)
=== RUN   TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused
    weave_abandon_force_teardown_test.go:80: abandon output:
        weave abandon: run #1 abandoned as rejected (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace $TMPDIR/TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused2282239533/001/.bashy/weave/002-bc521aad/workspaces/issue-1: teardown refused: workspace ref(s) hold commits in neither base nor a salvage ref: refs/heads/wip-pre-squash, refs/stash — `weave prune` reclaims it once resolved
--- PASS: TestWeaveAbandonNamesUnpreservedSideRefsWhenTeardownRefused (0.70s)
=== RUN   TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused
    weave_abandon_force_teardown_test.go:103: abandon output:
        weave abandon: run #1 abandoned as rejected (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: left alone: workspace $TMPDIR/TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused155709021/001/.bashy/weave/002-a23cfbae/workspaces/issue-1: teardown refused: run child termination remains unverified — `weave prune` reclaims it once resolved
--- PASS: TestWeaveAbandonForceNamesUnverifiedChildWhenTeardownRefused (0.39s)
=== RUN   TestWeaveAbandonForceSideRefCollisionNeverOverwritesSalvage
    weave_abandon_force_teardown_test.go:135: abandon output:
        weave abandon: run #1 abandoned as superseded (unmerged commits preserved at refs/salvage/abandoned-1)
        weave abandon: side work preserved at refs/salvage/abandoned-1-heads-wip-pre-squash-74b89f3e184617b49aed15e5c4f61218f101ff89
        weave abandon: side work preserved at refs/salvage/abandoned-1-heads-x-y
        weave abandon: side work preserved at refs/salvage/abandoned-1-heads-x-y-1832f9b7392ec05db2de6f6565588cd08d623a42
        weave abandon: side work preserved at refs/salvage/abandoned-1-stash
--- PASS: TestWeaveAbandonForceSideRefCollisionNeverOverwritesSalvage (1.83s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	5.748s
exit=0
```
