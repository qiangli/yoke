---
id: 36f430ffd3bd
kind: bug
title: weave reset silently ignores a run argument and wipes the whole queue
seq: 3051
status: todo
priority: p1
labels:
    - weave
    - safety
created: 2026-11-07T02:27:01.53066Z
---

Problem: 'bashy weave reset' (pkg/weave/weave_subverbs.go newWeaveResetCmd) tears down EVERY weave and removes all weave state for the project, but it declares no cobra Args rule, so 'bashy weave reset 3 --yes' silently ignores '3' and wipes the whole queue. Neighbouring verbs (pull, abandon, say, start --run N) are per-run, which makes the slip easy. Observed 2026-11-06 in Sprint 580: a manager meaning to restart one failed run cleared the umbrella queue, including two older terminal items and their workspaces.
Fix (part 1, required): Args: cobra.NoArgs on reset; the error names the per-run alternatives (weave abandon N, weave start --run N --resume) and says reset clears the whole queue. Test: 'reset 3' fails without touching queue state.
Fix (part 2, recommended): before tearing down, refuse when any item (including terminal history) holds commits not merged into the source branch (commits_ahead > 0 vs the live checkout) unless --force; list those items. Test with a temp repo.
Verification: GOWORK=off go test ./pkg/weave/ -run Reset -count=1.
