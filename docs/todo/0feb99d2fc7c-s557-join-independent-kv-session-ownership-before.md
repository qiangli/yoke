---
id: 0feb99d2fc7c
kind: bug
title: S557 join independent KV session ownership before temporary cleanup
seq: 3050
status: assigned
priority: p1
created: 2026-11-02T00:03:23.142151Z
assignee: s557-test-readiness
sprint: 557
sprint_id: f5ccf563-f5d1-5b94-8be4-9388e51ef86d
sprint_title: 'Bashy v1.0.0 preparation: split private/public development and retire qiangli/dhnt'
---

Native41359794189 Linuxstandalone Yoke ratchet7903pass1fail TestIndependentKVSharedOwner cmds/db: TempDirRemoveAll sessionsdirectorynotempty; confirmingrerunalsoFAIL. Yoke1c8269b9. /tmp/s557-native-125007549370.log. Diagnose actualbackgroundsessionowner andjoinbeforetempcleanup; no sleeps/baseline/skip/threshold changes. Isolatedworktree, boundedtests/race only nofullsuite/productbuildlocal. Preserve independentKV/sharedownerassertions; commitnopush.


## Delivery

The failing test started a real owner child but only deferred a `session stop` request. The successful wire response acknowledges admission shutdown; `Serve` subsequently drains request handlers, writes the final ended-session metadata, and closes its memory stores. Removing the temporary sessions directory immediately after the acknowledgment could race that final write. `startSession` deliberately releases its process handle because production owner sessions outlive their calling CLI; changing this daemon contract is unnecessary for the test.

The independent KV test now retains a process handle from the returned owner PID while the child is alive. Its cleanup asserts that stop succeeds, waits for that actual child, verifies successful process exit, and verifies final session metadata is `ended`. Cleanup is registered after the temporary directory and environment callbacks, so the owner is joined before either is removed/restored. A five-second shutdown watchdog kills and joins only the owned child on failure; malformed stop output also triggers owned-child cleanup. There are no sleeps, production protocol changes, removed assertions, or baseline changes. All create/bucket/write/replay/get checks are retained.

Validation: `GOWORK=off GOMAXPROCS=2 GOFLAGS=-p=2 go test -race ./cmds/db -run '^TestIndependentKVSharedOwner$' -count=3 -timeout=60s` passed on final source in 5.346s. The pre-fix bounded local selection also passed, so the hosted cleanup failure was not reproduced locally; the fix removes the proven missing owner join rather than relying on timing or retry. Hosted Linux acceptance remains required. Other CLI tests with detached owner sessions are outside this specific change and have not been claimed repaired.
