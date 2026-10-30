---
id: c839bb732f1a
kind: bug
title: 'S525.A10r yoke: restoring the OLDEST version from the history UI leaves the active app unchanged'
seq: 2936
status: done
priority: p0
labels:
    - app
created: 2026-10-29T20:40:45.040713Z
weave: 725
assignee: muse-conductor
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T20:53:53.925815Z
resolution: fixed
closed_by: muse-conductor-w
---

Part of S525.A10 (goal a10, yoke 3561440d), not a new feature. Live A9 browser-run on elephant WSL (bashy cd82bf78, yoke 5e6ce650) failed br.restored_page_old TWICE (2/2 runs, not timing): after alice approves a change through the real browser, the History UI's LAST 'Restore version <rev>' button (versions are newest-first, so the last one is the OLDEST, the base the app started from) is clicked; 5 s later /notes/ still contains the approved change (s525-br). Undo afterwards works (br.undo_page_back passes). The UI swallows any error into historyStatus(), and serve.log shows nothing, so the cause is unknown: likely candidates are a refusal from src.RestoreSealed / Gate.Check / checkReadable when the target is the base (pristine) version, or a restore that records a version but does not change what /notes/ serves. Steps: (1) reproduce in a unit/integration test through the same HTTP endpoint the UI calls (POST /api/shape/<app>/history/restore {rev:<oldest>}) after one approved change; assert the endpoint result AND that the served page no longer has the change. (2) Find the root cause and fix it so that restoring ANY listed version, including the oldest/base, takes effect (forward-only, user data untouched), or, if the base is deliberately not restorable, do not offer a Restore button for it and say why. (3) Make the UI show a refusal visibly (it already writes historyStatus; ensure the response is non-2xx on refusal). Gate: go test ./pkg/src ./pkg/shape/... ./pkg/webconsole -run 'TestShaped|History|Restore' + GOOS=windows go vet ./... . Keep it small.
