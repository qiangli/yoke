# Evidence — Sprint 467, Story #2366 (f0ca0d513925)

Worker commits in a weave workspace were recorded as `found identity
"unknown"` (or as the manager) by the commit-attribution check, which recorded
a bypass and auto-failed the sprint's manage score.

Candidate: yoke commit `73012f14` (parent `069bee29`, branch of run 397).
Holder: `s467-identity-fallback`. Tests run locally, short and filtered; the
whole-package gate is the manager's.

## Installed binary vs source

| item | value |
|---|---|
| prior worker-env fixes in source | `dc4e2b17` (2026-10-10), `d1ab26ce` (2026-10-12) |
| installed `bashy` build time | 2026-10-17 04:27 local |
| conclusion | no binary/source mismatch for the worker env; both fixes were live during the evidence runs |

## Live evidence (read-only, from the run 361 workspace and the sprint board)

| run 361 commit | time (local) | author | hook outcome |
|---|---|---|---|
| `47ed0d28` worker's own delivery, Sprint/Story trailers, no Agent trailer | 23:56:29 | agent-weave-issue-361 | no bypass on the board at 06:56Z — passed (BASHY_AGENT exported to the worker) |
| `491bad09` wrapper `weave(auto)` KB-only commit | 23:57:15 | agent-weave-issue-361 | board bypass at 06:57:15Z: `Story #2261 assignee "s453-sql-opus", found identity "unknown"` |
| `7526ffb2` manual correction | 00:07:34 | s453-sql-opus | — |

Run 397 itself: the wrapper's `wip(weave #397)` commit `069bee29` at 01:08:26
local matches the board bypass at 08:08:28Z (`Story #2366 ... found identity
"unknown"`). Sprint 449/457 show the other shape of the same defect:
`found identity "s449-manager"` / `"s457-manager"` when the conductor ran with
the manager's BASHY_AGENT exported.

Reading: the worker's own commits resolve; the WRAPPER's preservation commits
do not. `maybeAutoCommit` ran `git commit` under the conductor's environment,
and `weaveAppendRunProvenance` copied only Sprint/Story trailers from the
brief, so a historical audit (`--check-range`, pre-push), which reads the
Agent trailer alone, reports the preserved commit unknown as well.

## Red (parent 069bee29 + the three new tests, stub delegating to current code)

```
--- FAIL: TestMaybeAutoCommitStampsWorkerIdentityNeverLauncher (0.56s)
    weave_commit_identity_test.go:37: hook saw BASHY_AGENT="s467-manager", want the worker's identity
--- FAIL: TestAutoCommitProvenanceCarriesWorkerAgent (0.00s)
    --- FAIL: TestAutoCommitProvenanceCarriesWorkerAgent/owner/clean
        Agent trailer = "" (present=false), want the worker
    (same for owner/killed, launch/clean, launch/killed)
--- FAIL: TestAutoCommitMessagePassesAttributionUnderLauncherEnv (0.51s)
    weave_commit_identity_test.go:111: err=<nil> stderr=WARN commit attribution: Story #110 assignee "agent-a", found identity "s467-manager"; use Agent: agent-a (or BASHY_AGENT for a pending commit)
FAIL	github.com/qiangli/yoke/pkg/weave	1.792s
```

The third test drives the real `sprint commit-msg` verb against a scratch
board with a leased sprint and an assigned story, under a manager's
BASHY_AGENT — the conductor's situation — and reads the thread back.

## Green (candidate 73012f14)

```
go test ./pkg/weave -run 'TestMaybeAutoCommitStampsWorkerIdentityNeverLauncher|TestAutoCommitProvenanceCarriesWorkerAgent|TestAutoCommitMessagePassesAttributionUnderLauncherEnv' -count=1
ok  	github.com/qiangli/yoke/pkg/weave	1.962s
```

Regression, every test in the touched neighbourhood (commit attribution,
crash preservation, worker story submit, provision/reverify/submodule
auto-commit callers, child env, the new file): 50 tests, one filtered run.

```
ok  	github.com/qiangli/yoke/pkg/weave	88.170s
```

## Candidate's own delivery commit

`73012f14` was made by the worker with `Agent: s467-identity-fallback` and
passed the managed hook on the live board: `commit provenance: Sprint #467,
1 story reference(s) verified`, no bypass recorded.

## What changed, and what did not

- `maybeAutoCommitAs(workspace, msg, agent)` runs the wrapper's `git commit`
  with the launcher's BASHY_AGENT dropped and the worker's set
  (`weaveWorkerAgent`, the identity `weaveChildEnv` exports and the story claim
  uses). The three run paths (terminal auto-commit, forced abandon, forced
  prune) pass the worker. `maybeAutoCommit` keeps its signature and drops the
  launcher's identity too.
- `weaveAppendRunProvenance` appends `Agent: <worker>` to the copied
  Sprint/Story block, so historical audits resolve the preserved commit.
- Not changed: the hook never rewrites a worker's commit message (pinned by
  `TestCommitAttribution`), git author/committer names (a seat label, not
  attribution), merge-commit provenance, nested submodule salvage commits.

## Note for the merge

`069bee29` (the pre-fix wrapper commit on this branch, KB.md only) carries
no Agent trailer; a historical range audit will still list it. That is the
manager's call at merge time, not something this candidate can amend.
