---
id: 998bf06de8de
kind: enhancement
title: 'src: add ''src revert <repo>'' (reset local to pristine) and refuse ''src pr open'' when the head has no diff against base'
seq: 2038
status: done
priority: p3
created: 2026-10-23T18:03:59.637671Z
assignee: agy-gemini3.8-flash
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-24T19:09:27.792471Z
closed_by: s497-manager
---

Found in the 2026-10-23 Sprint 458 M2b live smoke. Reverting a host to the release currently needs raw git (checkout local; reset --hard pristine), and pr open accepted PR 1 with head == pristine (an empty diff). Both are agent-UX gaps; the M3 demo uses git for the revert.

## Worker brief (s497-manager, 2026-10-24). One repo: yoke.
Find the src commands with `yc search-symbols NewSrc yoke` / grep "src pr open". Deliver exactly: (1) `src revert <repo>`: reset the local src checkout to pristine (base tip), dry-run by default listing what would be discarded, `--apply` to do it; never touches branches/PRs of other repos. (2) `src pr open` refuses with a plain message when head has no diff against base. Red/green tests with temp BASHY_HOME and a temp git repo. No build outputs in commits (`go build -o /dev/null`). No new features beyond these two. Gate: go test for touched packages, go build -o /dev/null ./... . Commit yourself with this story's trailers.
