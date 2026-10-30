---
id: df339248d8c5
kind: bug
title: 'S525.A9w yoke: Windows api units refuse (procguard stub) and the delegate env lacks Windows profile vars, so shaping fails on native Windows'
seq: 2924
status: done
priority: p0
labels:
    - windows
created: 2026-10-29T17:38:11.876381Z
weave: 709
assignee: claude-opus5.5
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T17:47:01.119459Z
resolution: fixed
closed_by: claude-opus5.5-g
---

Sprint 525, goal a9 (parent eb1be776), p0. OS divergence on native Windows, found by the full A9 run (installed candidate bashy b044891d / yoke 583e4c7f, same scenario as Linux). The operator rule applies: any OS difference is a bug.

(1) pkg/procguard/guard_windows.go is a stub: Arm() always returns "procguard: kill-on-parent-exit is unsupported on windows". The default unit containment level (process) requires it, so EVERY shaped app api unit refuses on Windows. POST /notes/api/add gets 503 "unit add could not run as it asked (...): contain: level "process" unavailable: procguard: kill-on-parent-exit is unsupported on windows".
Fix: implement Arm with a Windows Job Object. Create the job with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE (plus BREAKAWAY_OK off) and assign the child right after start (or create it suspended, assign, then resume, so no grandchild escapes before assignment). Close the job handle in Disarm/when the parent exits, so the whole tree dies with the parent. ContainsSessionEscapes should report truthfully. Pure Go via golang.org/x/sys/windows; no admin, no setup.

(2) pkg/webconsole/shaped_delegate.go delegateEnvAllow (PATH HOME USER LOGNAME LANG LC_ALL TMPDIR TERM BASHY_HOME) is POSIX-only. On Windows the agent gets no USERPROFILE/APPDATA/LOCALAPPDATA/TEMP/TMP/SystemRoot, so genie fails with "chat: no state directory for reduced output" and every shaping request fails ("agent bashy exited 1").
Verified live: the same delegate with USERPROFILE, APPDATA, LOCALAPPDATA, TEMP, TMP, SystemRoot, PATHEXT, ComSpec added succeeds.
Fix: an OS-aware allow-list that adds exactly those Windows profile/system variables on windows; no secrets, no wholesale env. Check pkg/shape/contain's scrubbed unit env the same way: a bashy.exe unit needs SystemRoot (and TEMP/TMP) to run on Windows; keep it secret-free.

Acceptance:
- procguard: a windows-only test (runs where GOOS=windows) where an armed child's grandchild dies when the parent's job closes, plus a unit test of the env allow-list for both OSes.
- Run on Dragon: GOOS=windows go vet ./pkg/procguard ./pkg/shape/... ./pkg/webconsole; go test ./pkg/procguard ./pkg/shape/... ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair' green (darwin).
- The manager runs the native Windows test and the full A9 scenario on a Windows peer.
Do not touch the cookie/session code or scope.go. Sprint/Story/Story-ID trailers; no hostnames.
