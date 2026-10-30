---
id: a12ee91ebfc8
kind: bug
title: 'S525.A9d yoke: shaped delegate, unit runner and verifier exec ''bashy'' from PATH, not the running binary (old bashy ran, so every request failed)'
seq: 2922
status: done
priority: p0
labels:
    - software-shaping
created: 2026-10-29T17:12:58.518104Z
weave: 707
assignee: claude-sonnet5.5
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T17:29:36.976806Z
resolution: fixed
closed_by: claude-sonnet5.5-e
---

Sprint 525, goal a9 (parent eb1be776). Bug found by the live A9 smoke (Linux, elephant WSL, installed candidate bashy c7680732 / yoke e2842bb5). Every shaping request failed with:
  shaping agent: delegate: exit status 1: Error: unknown flag: --idle-timeout
The console ran the candidate bashy, but pkg/webconsole/shaped_delegate.go launches `bashy` from PATH (BashyDelegate.Bashy empty = "bashy"), which found an OLDER bashy installed in /usr/local/bin. The same PATH lookup happens for the shaped api unit runner (pkg/webconsole/shaped.go ~91, default {"bashy"}) and for the verifier's check runner (pkg/shape/verify/verify.go ~536, $BASHY_SHAPE_RUNNER else "bashy"). Mixed binaries mean wrong flags, wrong guards and wrong semantics, and a user can't know which one ran.

Fix: when no explicit binary is configured, all three resolve to the RUNNING bashy executable (os.Executable, symlinks resolved), the same binary that serves the console. $BASHY_SHAPE_RUNNER and an explicit field still override, so tests keep injecting their runner. Guard against the test binary: when os.Executable is not a bashy build (e.g. a go test binary), keep the current behaviour, so tests never re-exec themselves. Identical on every OS (bashy.exe on Windows).

Acceptance: a focused unit test of the resolver. Configured value wins; otherwise the running executable is used when it identifies as bashy; otherwise "bashy". Plus a test showing the delegate argv uses that path. go test ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair' ./pkg/shape/... green; GOOS=windows go vet. Do not edit scope.go or pkg/fleet (S525.A9b in parallel), or the cookie/session code (S525.A9c in parallel). Sprint/Story/Story-ID trailers; no hostnames.
