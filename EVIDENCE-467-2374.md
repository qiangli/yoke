# Sprint 467 story 2374: unread hint latency

## Reproducer and red result

On a synthetic `posts.jsonl` with 12,000 old 2 KiB posts, reader cursor 1000,
and one new directed arrival at seq 12001, the old hint path called the
authoritative `Unseen` reader. It decoded the long unread backlog even though
only one recent post concerned the reader.

Command (before the fix):

```text
BASHY_HINTS=off go test ./pkg/bus -run '^TestUnseenOldCursorNewArrivalIsBounded$' -count=1 -v
=== RUN   TestUnseenOldCursorNewArrivalIsBounded
    board_test.go:46: old cursor + one new arrival after 12000 posts: 578.646458ms
    board_test.go:51: old cursor hint took 578.646458ms; want at most 40ms
--- FAIL: TestUnseenOldCursorNewArrivalIsBounded (1.02s)
FAIL
```

An installed `bashy mb --peek -n 1` on the populated host also ran for over
30 seconds before I interrupted it; it did not advance the board cursor.
This was a diagnostic read, not the synthetic timing gate.

## Fix and green results

`bus.HintUnseen` reads at most the final 64 KiB for the advisory hint and
reports when older unread records may exist. The bashy local hint caller uses
it and renders an honest `at least` or generic prompt when the sample is
incomplete. Explicit inbox/MB reads still use authoritative `Unseen`; hints
never call the acknowledgement closures or move cursors. The bashy caller was
changed only in isolated worktree `/tmp/s467-2374-bashy`.

Commands and raw timing output:

```text
BASHY_HINTS=off go test ./pkg/bus -run '^(TestHintUnseenOldCursorNewArrivalIsBounded|TestBoard_)' -count=1 -v
=== RUN   TestHintUnseenOldCursorNewArrivalIsBounded
    board_test.go:46: old cursor + one new arrival after 12000 posts: 486.25µs
--- PASS: TestHintUnseenOldCursorNewArrivalIsBounded (0.50s)
PASS
ok  github.com/qiangli/yoke/pkg/bus  1.772s

BASHY_HINTS=off GOWORK=/tmp/s467-2374-go.work go test ./internal/agentos -run '^(TestLocalUnreadHintOldCursorStaysBounded|TestSummarizeUnreadHintAttribution|TestUnreadHintNeverConsumesMail|TestUnreadHintIsolatesInstances|TestUnreadHintRetiredPrincipalStaysSilent|TestUnreadHintSkipsReadersAndSwitches)$' -count=1 -v
=== RUN   TestLocalUnreadHintOldCursorStaysBounded
    inbox_hint_test.go:109: local unread hint, old cursor + one new arrival after 12000 posts: 1.742625ms
--- PASS: TestLocalUnreadHintOldCursorStaysBounded (0.09s)
PASS
ok  github.com/qiangli/bashy/internal/agentos  2.283s
```

The test also verifies the full `Unseen` result still includes the directed
post, caught-up readers stay quiet, an incomplete empty sample remains honest,
and the hint leaves the reader cursor unchanged. Only filtered tests ran on
the local host; the manager owns remote package gates.

Final local bus gate after the scoped code commits:

```text
BASHY_HINTS=off go test ./pkg/bus -run '^(TestHintUnseenOldCursorNewArrivalIsBounded|TestUnseenAtHeadOnPopulatedBoardIsBounded|TestUnseenTailProbeKeepsMalformedAndPartialTailSemantics|TestUnseenTailProbeFallsBackForLargePost)$' -count=1 -v
=== RUN   TestUnseenAtHeadOnPopulatedBoardIsBounded
    board_hint_latency_test.go:36: populated board: 12000 posts, 25512894 bytes, Unseen at head 2.818666ms
    board_hint_latency_test.go:59: populated board: one new post after 12000 old posts, Unseen 1.205541ms
--- PASS: TestUnseenAtHeadOnPopulatedBoardIsBounded (0.19s)
=== RUN   TestUnseenTailProbeKeepsMalformedAndPartialTailSemantics
--- PASS: TestUnseenTailProbeKeepsMalformedAndPartialTailSemantics (0.01s)
=== RUN   TestUnseenTailProbeFallsBackForLargePost
--- PASS: TestUnseenTailProbeFallsBackForLargePost (0.01s)
=== RUN   TestHintUnseenOldCursorNewArrivalIsBounded
    board_test.go:46: old cursor + one new arrival after 12000 posts: 8.517666ms
--- PASS: TestHintUnseenOldCursorNewArrivalIsBounded (0.63s)
PASS
ok  github.com/qiangli/yoke/pkg/bus  1.260s
```

Final isolated bashy caller gate:

```text
BASHY_HINTS=off GOWORK=/tmp/s467-2374-go.work go test ./internal/agentos -run '^(TestLocalUnreadHintOldCursorStaysBounded|TestSummarizeUnreadHintAttribution|TestUnreadHintNeverConsumesMail|TestUnreadHintIsolatesInstances|TestUnreadHintRetiredPrincipalStaysSilent|TestUnreadHintSkipsReadersAndSwitches)$' -count=1 -v
=== RUN   TestSummarizeUnreadHintAttribution
--- PASS: TestSummarizeUnreadHintAttribution (0.00s)
=== RUN   TestLocalUnreadHintOldCursorStaysBounded
    inbox_hint_test.go:109: local unread hint, old cursor + one new arrival after 12000 posts: 49.334334ms
--- PASS: TestLocalUnreadHintOldCursorStaysBounded (0.17s)
=== RUN   TestUnreadHintNeverConsumesMail
--- PASS: TestUnreadHintNeverConsumesMail (0.13s)
=== RUN   TestUnreadHintIsolatesInstances
--- PASS: TestUnreadHintIsolatesInstances (0.13s)
=== RUN   TestUnreadHintRetiredPrincipalStaysSilent
--- PASS: TestUnreadHintRetiredPrincipalStaysSilent (0.20s)
=== RUN   TestUnreadHintSkipsReadersAndSwitches
--- PASS: TestUnreadHintSkipsReadersAndSwitches (0.09s)
PASS
ok  github.com/qiangli/bashy/internal/agentos  4.007s
```
