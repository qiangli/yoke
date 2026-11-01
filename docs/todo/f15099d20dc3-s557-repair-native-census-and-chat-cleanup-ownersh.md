---
id: f15099d20dc3
kind: bug
title: S557 repair native census and chat cleanup ownership
seq: 3048
status: assigned
priority: p1
created: 2026-11-01T19:27:07.399381Z
weave: 1
assignee: s557-manager
sprint: 557
sprint_id: f5ccf563-f5d1-5b94-8be4-9388e51ef86d
sprint_title: 'Bashy v1.0.0 preparation: split private/public development and retire qiangli/dhnt'
---

Sprint557 migration repair. Work only in assigned isolated workspace. Read local CLAUDE/AGENTS. Preserve all test assertions, coverage, thresholds and deadlines; diagnose before modifying. No release/push/parent pin changes. No heavy/full local builds/suites: bounded targeted tests only GOMAXPROCS=2 GOFLAGS=-p=2, full validation hosted by manager. Commit named files with Sprint: #557 and correct Story/Story-ID trailers and injected BASHY_AGENT. Report exact evidence and limitations. Native run41332670393 Ubuntu workspace: ratchet census omits pkg/nativepath and pkg/nfthelper; chat TestChatGrantSettlesACPPermissionRequest tempdir cleanup directory-not-empty (isolated retry passed). Inspect actual package census; repair missing entries without reducing checks; investigate ownership/worker shutdown before cleanup and add focused regression. Logs /tmp/s557-native-ubuntu-workspace-41332670393.

## Manager ownership rework (2026-11-01)

Reworked external checkpoint 1965611d without the unrelated KB commit. The
checkpoint's 3-second relay join and 2-second SDK Done wait did not establish
ownership. SDK Done signals reader shutdown before queued callbacks drain.

- Serialize session relay admission against shutdown; refuse late startup after
  a natural exit. Join synchronous preparation and check cancellation before
  delivery. Do not abandon an in-flight snapshot or change bus callback APIs.
- Cancel and join relay delivery, including a blocked control socket write.
  Interact joins its relay before deferred room cleanup. PTY completion joins
  before budget/room/inbox/TTY cleanup; explicit Close waits for that barrier
  after its existing graceful window and process cancellation.
- Track ACP prompt and handler admissions, cancel their owned contexts, reject
  new admissions on shutdown, and join existing work before room/budget cleanup.
  Queued SDK notifications arriving after shutdown are refused rather than
  accessing released host state. No claim that SDK reader Done joins callbacks.
  The upstream SDK may finish internal diagnostics after Close; host callbacks
  can no longer write after the ownership barrier.
- Preserve census additions, all test assertions and deadlines. Replace the
  early-done plus sleep relay test with deterministic in-flight barriers.

Bounded verification, GOWORK=off GOMAXPROCS=2 GOFLAGS=-p=2:

- go test -race -count=1 -timeout=90s -run
  '^(TestSessionRelay|TestACPClose|TestACPLifetime|TestInboxRelay|TestACPSessionIsDriven|TestACPSessionNotices|TestChatGrantSettles|TestACPUpdatesMap|TestACPPermissionKeeps|TestClosePTYForce|TestAbortStart)'
  ./pkg/chat: PASS, 7.453s.
- go test -race -count=1 -timeout=30s -run
  '^TestControlWriteCancellationJoinsBlockedWrite$' ./pkg/agentpty: PASS, 1.264s.
- git diff --check: PASS. No full suite/build, push or parent pin changes.

Remaining separate triage: the macOS Linux-engine graph fixture inherits remote
GOFLAGS despite selecting the native Linux engine. Its graph query needs an
explicit native build-tag contract. The delegate heartbeat's 150ms failure is
retained; its five 40ms shell sleeps do not distinguish scheduling stalls from
post-output cleanup latency. No timeout or assertion weakening was applied.
