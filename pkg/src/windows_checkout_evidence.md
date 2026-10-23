# Sprint 458, Story 2039: Windows source checkout

The focused test ran on the approved Windows amd64 host with Go 1.27.0.
The three cases cover a fresh fetch at a present commit, a repeat fetch of
an existing Windows clone, and a local PR merge. The fixture records an
executable file in Git while its Windows worktree cannot retain that bit.
Temporary files were rooted at `C:\s458tmp` so the output contains no user
path. The red run used the original implementation; the green run used the
changes in this story.

Red, `go test ./pkg/src -run TestWindows -count=1 -v` (exit 1):

```text
=== RUN   TestWindowsFetchExecutableSeedAndRepeat
    windows_checkout_test.go:83: fetch published commit 3f232617f77de40116b34c0c1cc92b157760d91a: src: refusing yoke: commit 3f232617f77d not found in C:\s458tmp\TestWindowsFetchExecutableSeedAndRepeat2206146647\001\seed\yoke.git (checkout: reference not found)
--- FAIL: TestWindowsFetchExecutableSeedAndRepeat (0.15s)
=== RUN   TestWindowsFetchExistingNativeClone
    windows_checkout_test.go:99: fetch existing clean Windows clone: src: checkout pristine: checkout: worktree contains unstaged changes
--- FAIL: TestWindowsFetchExistingNativeClone (0.16s)
=== RUN   TestWindowsPRMergeExecutableCheckout
    windows_checkout_test.go:163: merge PR in clean Windows clone: src: checkout local: checkout: worktree contains unstaged changes
--- FAIL: TestWindowsPRMergeExecutableCheckout (0.22s)
FAIL
FAIL	github.com/qiangli/yoke/pkg/src	2.704s
FAIL
```

Green, same command (exit 0):

```text
=== RUN   TestWindowsFetchExecutableSeedAndRepeat
--- PASS: TestWindowsFetchExecutableSeedAndRepeat (0.29s)
=== RUN   TestWindowsFetchExistingNativeClone
--- PASS: TestWindowsFetchExistingNativeClone (0.18s)
=== RUN   TestWindowsPRMergeExecutableCheckout
--- PASS: TestWindowsPRMergeExecutableCheckout (0.27s)
PASS
ok  	github.com/qiangli/yoke/pkg/src	2.853s
```

The final Windows binary fetched the published release commit into a fresh
source home, fetched it again as a no-op, created a topic branch, opened a PR
with a passing `go test ./pkg/src -run TestShowCoreSkillExternal -count=1`,
merged it into `local`, and reported `modified` in doctor. After checking out
`pristine` and moving `local` back to the release commit, both `src status`
and doctor reported `pristine`.

Additional gates: macOS `go test ./pkg/src ./git -count=1`, `go build ./...`,
and `go vet ./...` passed. Linux `go test ./pkg/src -count=1` passed on the
approved guest. A full Windows `go test ./pkg/src -count=1` still fails in
`TestM2MergeRefusesFailingTest`: that existing test uses `true` as its passing
test command, which Windows `cmd` cannot run. The broad Windows `pkg/skills`
test also has existing platform path failures; the smoke PR's focused test
passed.
