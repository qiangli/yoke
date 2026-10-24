---
id: 10308a1275df
kind: bug
title: 'M3w (yoke) pkg/src on Windows: src fetch reports ''commit not found'' for a published commit and fails re-checkout; src pr merge fails checkout'
seq: 2039
status: done
priority: p0
created: 2026-10-23T18:59:00.448481Z
weave: 616
assignee: codex-gpt6-sol
sprint: 458
sprint_id: 3b16e60f-dce3-5dd1-8ed1-4384110d0741
sprint_title: 'bashy is all you need, MVP: invariant core + every other command as editable Bash# skill/script'
closed: 2026-10-23T19:21:36.188716Z
resolution: fixed
closed_by: codex-gpt6-sol-r
---

Found in M3 (1838200c), 2026-10-23; evidence: umbrella docs/sprint-458-evidence.md, "Windows amd64". On the approved Windows host, with an M2b bashy.exe built from bashy 2496a10a (yoke 44a44a7e):
1. `bashy src fetch yoke` -> "src: refusing yoke: commit 44a44a7e31d1 not found in https://github.com/qiangli/yoke (checkout: reference not found)". The commit is on main.
2. Against a native git clone at 44a44a7e, fetch "still failed when it tried to re-check out the same branch", while native git reported a clean worktree.
3. `src pr merge 2` refused on checkout.
macOS and Linux pass the same flow. Likely causes to check first: the pure-Go git clone on Windows does not fetch the full history or the ref (shallow or single-branch), path separators or CRLF making the worktree look dirty to the pure-Go status, and the read-only file mode (0444) from protectFiles blocking checkout on Windows, where read-only files cannot be overwritten.
Repo: yoke only (pkg/src, plus the yoke git package if the root cause is there). Public: no hostnames or user ids.
Acceptance, red/green: a unit test reproducing each failure that runs under GOOS=windows on the approved Windows host (run the focused go test there through the existing remote-run path; record the exact output), then the fix; go test ./pkg/src on macOS/Linux still green; go build ./... + vet. Then rerun the M3 Windows smoke (fetch, branch, PR, merge, doctor modified, revert to pristine) with the fixed binary and append the result to the umbrella evidence (the manager integrates the umbrella part).
