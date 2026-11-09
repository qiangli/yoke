---
id: dfa47824dc82
kind: bug
title: 'shape: agent binary path and absolute workspace paths leak into the UI and commit trailers'
seq: 3066
status: todo
priority: p1
labels:
    - shaping
    - privacy
created: 2026-11-09T08:20:03.750858Z
---

Found rehearsing the software-shaping demo (examples/shaping/hello-world, codex:gpt6-sol, 2026-11-09).

Problem: shaping exposes the operator's home directory, and with it the OS username, in three places.
1. Candidate card and Versions list: "by <home>/Library/Caches/bashy/bin/codex/<ver>/bin/codex". DelegateResult.Agent takes the delegate envelope's `agent` field, which is the resolved binary path (pkg/shape/delegate.go:104), and webconsole/artifact/shape.js:65 renders it.
2. Shaped version commits: helpers.go:174 writes "Shape-Agent: " + c.Agent into the commit message, so the binary path is in git history; `shape submit`/`push` can carry it to org or upstream. submit.go:153 adds "Agent: " + agent the same way (check which value it holds).
3. Coverage text ("Does:", genie.js:967, and the Versions list): the agent's free-text summary contains absolute candidate-workspace paths (<BASHY_HOME>/shape-work/<app>/<id>/work/public/index.html:6) as markdown links.

Expected: record and show the agent as its tool:model nickname (or agent identity, e.g. genie-delegate:codex-gpt6-sol), never a filesystem path; keep the resolved binary only in local, unexported logs if needed. Rewrite paths under the candidate workspace to app-relative paths (public/index.html:6) in coverage text before storing or rendering. Commit trailers carry only the nickname.

Verification: rerun the hello-world web and CLI walkthroughs; the candidate card, review page, Versions list, `shape show --json` and `git log` of the shaped line contain no absolute home or workspace paths. Add a unit test for the agent label and for workspace-path rewriting in coverage.

Related (isolation, separate): with a throwaway BASHY_HOME, each wish still writes its tracking todo under the real ~/.bashy/todo/<caller>/ (tracking_root ignores BASHY_HOME).

Also seen: `shape wish --from DIR` run with the shell inside an unrelated git checkout wrote its tracking todo and .shape-tracking.lock into that checkout's docs/todo/. Shaping tracking should land in the app's or BASHY_HOME's store, never in the caller's current repository.
