# `bashy weave` runbook — three agents, three issues, end to end

> **Status (2026-06-10):** the MVP flow described here works **today**
> against the local backend (`queue.json` + `git worktree` + `git
> merge --no-ff` — no Gitea, no `ycode serve` needed). The
> Gitea/mirror/merger/sticky-comment surface this runbook also
> describes is the deferred v2.1 design from
> [`loom-v2-plan.md`](./loom-v2-plan.md); use it as the target
> roadmap, not as the current behavior.
>
> For an MVP-flow quick-reference an agent can `get_doc` at
> runtime, see [`internal/docs/agent/weave.md`](../internal/docs/agent/weave.md).
> Key MVP guarantees, all e2e-tested:
>
> - Subagent runs inside a freshly-allocated PTY by default (so
>   claude-code / codex / opencode / aider render correctly).
> - When parent stdin isn't a TTY (orchestrator pipe, backgrounded
>   via `&`), PTY output is captured to a per-issue log file and
>   the path is in the result envelope.
> - Backgrounded `weave start` auto-`setsid`s, surviving the
>   launching shell's exit without `nohup`/`disown`.
> - State transitions on tool exit: `submitted` (rc=0) or `failed`
>   (rc≠0) with `exit_code` + `log_path` persisted.
> - `weave wait [--issue N | --all] [--timeout DUR]` blocks until
>   target reaches terminal state; pairs cleanly with `weave pull`.
> - Watchdogs on `weave start`: `--idle-timeout` (no PTY output),
>   `--max-runtime` (hard wall-clock cap, immune to spinner
>   output), `--mem-limit` (subagent-tree RSS budget, default
>   `16g` — the OOM backstop). All three kill the subagent's whole
>   process tree, and the wrapper forwards SIGTERM/SIGINT/SIGHUP
>   to the tree, so `weave kill`/`abandon` can't orphan a subagent.
>   Pointed work derives `--max-runtime` automatically
>   (1=3m45s, 2=7m30s, 3=11m15s, 5=18m45s, 8=30m); an explicit
>   value may tighten but cannot exceed that cap. Sprint links reject
>   missing or invalid points.
> - `weave start` refuses an issue whose previous wrapper is still
>   alive; `weave list` flags `working` items whose wrapper died
>   (`stale` in JSON, `*` in the table).
> - `weave start` also refuses a named fleet AGENT that is already
>   working another run — an agent is one identity (one store, one
>   kb attribution, one bus cursor), so two live runs under it mix
>   context. The run stays `todo` for it to pick up next. Pass
>   `--clone` to mint a per-issue ephemeral agent (`007-w3`) with
>   its own name and context and run them in parallel instead.
>   (Bare tool names are unaffected: `-- codex` is not an identity,
>   so the three-terminal walk-through below still works as written.)

Worked example: one human user, three issues, three different agentic CLIs, parallel execution, converged result pushed to GitHub. Use this as the canonical walk-through when teaching the feature or testing it end-to-end.

See [`loom-v2-plan.md`](./loom-v2-plan.md) for the design rationale behind every step below.

## Scenario

Alice has cloned `github.com/alice/myapp` locally to `~/projects/myapp/`. She has three pieces of work — fix issue #123, refactor for #124, add the feature in #125 — and wants Codex on #123, OpenCode on #124, and Claude Code on #125 to work them in parallel. End state: three commits sitting on her local `main`, ready to push to GitHub.

This runbook assumes ycode is installed (`brew install ycode` or equivalent) and the three agent CLIs (`codex`, `opencode`, `claude-code`) are on `$PATH`.

## Phase 0 — One-time setup (~30 seconds)

### 0.1 Start `ycode serve`

`weave` needs the embedded Gitea + merger running. Either run `ycode serve` in a background terminal or let it auto-start on first `weave` call. For this runbook:

```bash
cd ~/projects/myapp
ycode serve --background
```

Expected output:

```
ycode serve: starting on 127.0.0.1:5743
manifest: ~/.agents/ycode/manifest.json
gitea:    http://127.0.0.1:5743/gitea/
ready.
```

Verify health:

```bash
ycode serve status
# → ok (gitea, merger, mcp all green)
```

### 0.2 (Optional) selfinit your agent tools

If you haven't yet, run:

```bash
ycode init
```

This registers ycode's MCP servers and the `weave`-aware launch hooks into `~/.config/claude-code/`, `~/.config/codex/`, `~/.config/opencode/`. Each tool, after restart, knows to behave correctly under auto-attach (refuse to run unmanaged, read `YCODE_LOOM_ISSUE_BODY`, etc.). Skip this step if your tools don't support MCP — `weave` still works, you'll just feed prompts manually in Phase 2.

## Phase 1 — Seed the queue (and trigger first-run setup)

You file issues into the queue with `bashy weave add`. The very first `weave add` in this repo bootstraps the whole Gitea side; subsequent ones are sub-second.

### 1.1 First `add` (first-run setup happens implicitly)

```bash
bashy weave add "fix null deref in cache" --priority p0 --body "Stack trace in #log/2026-06-09.log; reproduces on cold cache."
```

Expected output (first run only):

```
weave: first run in this project — setting up
  ✓ mirror   admin/myapp into local Gitea
  ✓ labels   loom:working/submitted/ci-failed/conflict/merged/abandoned
             loom:p0/p1/p2/p3
             loom:source:human, loom:source:agent
  ✓ templates  .gitea/issue_template/{bug,task}.md
  ✓ hook     pre-commit installed in .git/hooks/
  ✓ config   wrote .ycode/loom.yaml (backend: forge, identity: ephemeral, default_tool: codex)
weave add: issue #123 created (priority p0, source human, label loom:todo)
```

### 1.2 Add the rest

```bash
bashy weave add "refactor user service for testability"
bashy weave add "add dark mode toggle" --priority p2
```

```
weave add: issue #124 created (priority p2, source human, label loom:todo)
weave add: issue #125 created (priority p2, source human, label loom:todo)
```

Three issues are now queued (each carrying `loom:todo`), sorted by priority: #123 (p0) is top, then #124 and #125 FIFO by creation time within p2.

### 1.3 (Optional) Let an agent rank the queue

If you'd rather hand the prioritization to an LLM:

```bash
bashy weave prio --auto
```

```
weave prio --auto: re-ranked 3 issues
  #123  p0 (was p0)  "fix null deref in cache"
  #124  p1 (was p2)  "refactor user service for testability"
  #125  p2 (was p2)  "add dark mode toggle"
```

Or set one explicitly:

```bash
bashy weave prio 124 p1
```

### 1.4 (Optional) Inspect the queue

```bash
bashy weave open --issues
# opens http://127.0.0.1:5743/gitea/admin/myapp/issues?labels=loom:todo&state=open
```

You'll see all three issues in Gitea's list view, color-coded by priority label. Filter further by clicking labels in the sidebar; the URL bar carries the filter state if you want to bookmark a particular view.

If you prefer a kanban: run `bashy weave init-board` once (it's a separate, opt-in setup that creates a Gitea project board via the web routes). Loom does not auto-sync card positions, so if you go that route you'll drag cards manually. For most workflows the filtered list view is enough.

To preview what the next `weave start` will claim:

```bash
bashy weave next
# → #123  p0  "fix null deref in cache"  (picked_by: priority)
```

## Phase 2 — Start three weaves, one per terminal

Open three terminals. One command each. **No `--issue` flag** — `weave start` claims the top of the queue atomically.

### Terminal 1 — Codex picks #123 (the p0)

```bash
cd ~/projects/myapp
bashy weave start -- codex
```

Expected output:

```
weave: claimed issue #123 "fix null deref in cache" (p0, top of todo)
weave: workspace at ~/.agents/ycode/gitea/loom/workspaces/ab12cd34/
weave: launching codex...
```

### Terminal 2 — OpenCode picks #124 (the p1)

```bash
cd ~/projects/myapp
bashy weave start -- opencode
```

```
weave: claimed issue #124 "refactor user service for testability" (p1, top of todo)
weave: workspace at ~/.agents/ycode/gitea/loom/workspaces/ef56gh78/
weave: launching opencode...
```

### Terminal 3 — Claude Code picks #125 (the p2)

```bash
cd ~/projects/myapp
bashy weave start -- claude-code
```

```
weave: claimed issue #125 "add dark mode toggle" (p2, top of todo)
weave: workspace at ~/.agents/ycode/gitea/loom/workspaces/ij90kl12/
weave: launching claude-code...
```

Three agents now run in three isolated workspaces. The atomic-claim guarantee meant each `weave start` picked a different card even though the calls were near-simultaneous. None of the workspaces can see the others' files, branches, stashes, hooks, or in-progress commits (per the workspace-isolation invariant in the plan doc).

### Even simpler: omit the tool too

If you don't pass `-- <tool>`, `weave start` resolves the tool from the issue's `tool:X` label (if any), then from `default_tool` in `.ycode/loom.yaml`. So if every issue should go to `codex`:

```bash
bashy weave start              # claims top of todo, uses default_tool
bashy weave start              # claims next, same tool
bashy weave start              # claims next, same tool
```

Three terminals × one bare command. This is the minimum-friction shape.

### What each agent sees

Inside its workspace, each tool has:

- `cwd` = the workspace path (a full `git clone --reference` of myapp).
- Environment:
  - `YCODE_LOOM_ID=loom-...`
  - `YCODE_LOOM_BRANCH=agent/agent-loom-issue-123-.../free-...`
  - `YCODE_LOOM_BASE=main`
  - `YCODE_LOOM_ISSUE=123`
  - `YCODE_LOOM_ISSUE_TITLE="fix null deref in cache"`
  - `YCODE_LOOM_ISSUE_BODY="<the issue body>"`
- MCP registry filtered to the **sub-agent role**: `loom_checkpoint`, `loom_submit`, `loom_abandon`, plus the `loom://session` resource. The agent does not see `loom_open` or any of the parent verbs.

If the tool is selfinit-registered, it auto-reads `YCODE_LOOM_ISSUE_BODY` and starts working. If not, you'll need to give it a starter prompt — the title and body are right there in env vars:

```
codex> work on the issue described in $YCODE_LOOM_ISSUE_BODY; the title is "$YCODE_LOOM_ISSUE_TITLE"
```

## Phase 3 — Monitor (passive, optional)

You can let the agents run untouched. To peek at progress, two options.

### Option A — Terminal: `bashy weave list`

In any fourth terminal:

```bash
bashy weave list --watch
```

Live-updating TUI:

```
ISSUE  PRIO  SOURCE  TOOL          STATE          WORKSPACE                       HEARTBEAT  ACTION
#123   p0    human   codex         working        .../workspaces/ab12cd34        2s         editing
#124   p1    human   opencode      submitted      .../workspaces/ef56gh78        4m         CI running
#125   p2    human   claude-code   working        .../workspaces/ij90kl12        1s         editing
```

`--watch` streams state transitions over the `loom://project` MCP resource — no polling.

To watch what a specific subagent is printing (any number of watchers, human or agent):

```bash
bashy weave log 123 -f          # stream issue #123's PTY capture live
bashy weave log 123 -n 100      # just the last 100 lines
```

The capture is recorded whenever the `weave start` parent wasn't a TTY (orchestrator pipe, `&`); it persists after the run as the post-mortem artifact. Tools that buffer in non-interactive modes (`claude -p`) leave it empty until exit.

You can also steer a running subagent — `weave say` types a line into its PTY (keystrokes + Enter, via the wrapper's control socket):

```bash
bashy weave say 123 "/btw what's your status? summarize in one line"
bashy weave say 123 "stop exploring; commit what passes and exit"
bashy weave log 123 -f          # watch the reaction
```

One say = one submitted line. The issue must be `working` with a live wrapper; tools that ignore terminal input in their non-interactive modes (`claude -p`) receive but ignore the keystrokes — use a TUI/streaming mode for steerable runs.

### Option B — Browser: Gitea filtered issue list

```bash
bashy weave open --issues
```

Gitea's issue list shows the three issues with their current state labels (`loom:working` / `loom:submitted` / etc.) color-coded. Refresh as work progresses; click any issue → standard Gitea issue page with comments, PR link, CI status, sticky loom comment showing workspace path and heartbeat. Filter URLs (`?labels=loom:working`, `?labels=loom:p0`) carry the bookmark state.

For programmatic monitoring (e.g., from a script or higher-level orchestrator):

```bash
bashy weave list --watch --json | jq -c '. | {issue: .result.issue, from: .result.from, to: .result.to}'
# {"issue":124,"from":"working","to":"submitted"}
# {"issue":124,"from":"submitted","to":"merged"}
# ...
```

## Phase 4 — Convergence (autonomous)

As each agent finishes its work, it calls `loom_submit`. (If a tool exits cleanly without calling submit, the wrap layer auto-submits its workspace state — selfinit-installed handler.)

What happens behind the scenes for each:

1. Branch pushed to local Gitea.
2. PR opened against `main`. Issue label flips `loom:todo` → `loom:submitted`.
3. Merger picks up the PR, rebases onto current `main`.
4. CI runs (whatever's in `.ycode/loom.yaml` `ci_command`, or auto-detected from your project — `make test`, `npm test`, etc.).
5. Green CI → fast-forward merge into local Gitea's `main`. Card moves to `merged`. Issue auto-closes via `Fixes #N` trailer.

The first PR in is trivial; subsequent PRs may need a rebase (handled silently if no conflict) or surface a `conflict` state for the originating agent to resolve in-place.

### What if there's a conflict?

Suppose #123 and #124 both touch `internal/cache/cache.go`. The merger:

1. Merges #123 first (it arrived first).
2. Tries to rebase #124 → conflict.
3. Rebases #124's branch onto new `main` in the workspace, leaves conflict markers in `cache.go`.
4. Issue label flips to `loom:conflict`.
5. `loom_submit` returns `{state: conflict, files: ["internal/cache/cache.go"]}` to the agent.
6. Agent edits the conflicted file using its normal tools, calls `loom_submit` again.
7. Loop until green.

If the agent already exited (some tools stop after a `loom_submit`), you can reattach:

```bash
bashy weave start --resume --issue 124 -- opencode
```

This drops a fresh OpenCode session into the same workspace, where the conflict is sitting ready to resolve.

If you want to fix it yourself:

```bash
bashy weave shell 124
# drops into a shell already in the workspace with author identity active
$ git status
# you see the conflict
$ $EDITOR internal/cache/cache.go
$ git add internal/cache/cache.go
$ bashy weave submit 124         # CLI shortcut for loom_submit
```

### What if CI fails?

Card moves to `ci_failed`. The merger posts a comment on the PR with the failing job name and last 200 lines of output. Same recovery model: the agent (or you, via `weave shell`) fixes and re-submits.

## Phase 5 — Pull converged work back

Your original `~/projects/myapp/` checkout has been completely untouched throughout — that's Guarantee 1 of the design. The merger has been advancing the local Gitea's `main` as each PR merged. Now you absorb that work.

### 5.1 Check state

```bash
bashy weave list
```

```
ISSUE  PRIO  TOOL          STATE     PR    MERGED AT
#123   p0    codex         merged    !1    2 min ago
#124   p1    opencode      merged    !2    1 min ago
#125   p2    claude-code   merged    !3    30s ago
```

All green. (If anything is still `working` or `conflict`, address it before pulling.)

### 5.2 Fast-forward your local main

```bash
cd ~/projects/myapp
bashy weave pull
```

Expected output:

```
weave pull: stashing 0 uncommitted changes
weave pull: fast-forward main from a3f9b2 to 8c41e5
  + 8c41e5 fix null deref in cache (#123)
  + 7d22f1 refactor user service for testability (#124)
  + 2b94a8 add dark mode toggle (#125)
weave pull: 3 commits absorbed, 0 conflicts (always — pull is always FF)
weave pull: re-applying stash (none)
```

If you'd had uncommitted edits in your checkout, they'd be stashed before the FF and re-applied after. If you'd had *committed* local edits diverging from Gitea's main, `weave pull` would refuse and tell you to either rebase or merge manually — it never silently rewrites your local commits.

### 5.3 Verify

```bash
git log --oneline -5
```

```
8c41e5 (HEAD -> main) fix null deref in cache (#123)
7d22f1 refactor user service for testability (#124)
2b94a8 add dark mode toggle (#125)
a3f9b2 <previous commit>
...
```

Three commits, properly attributed (committers are `agent-loom-issue-123-...@ycode.local` etc.), all clean, all on `main`. Each commit's body contains `Fixes #N` for the local Gitea issue plus a footer linking the original workspace lease and the merge PR for traceability.

## Phase 6 — Push to GitHub

Standard git from here. Nothing weave-specific.

```bash
git push origin main
```

Expected:

```
To github.com:alice/myapp.git
   a3f9b2..8c41e5  main -> main
```

Done. Three issues' worth of work, three different agents, one push.

## Phase 7 — Cleanup (mostly automatic)

The reaper handles most of it:

- Merged leases are torn down within the grace window. Workspace directories removed. Branches preserved in Gitea for audit (deletable later via `ycode loom prune` if you care about disk).
- Closed (merged) issues remain in Gitea with their `loom:merged` label as history; Gitea's default issue list filter hides closed by default but they're a click away.

If you want to nuke everything for this project explicitly:

```bash
bashy weave reset
# Confirm: remove 3 leases, 3 branches, 0 workspaces (all reaped), preserve labels and issues? [y/N]
```

## Common situations cheat sheet

### A merge is running and you need the board

You do not have to wait, and you must never kill the autopilot to get in.
`weave list`, `weave show`/`status`, `weave add` and `weave comment` all work
while a `weave pull --review-agent` cycle is in flight: reads are lock-free and
writes take the queue lock only for the milliseconds of their own mutation. An
issue you file mid-merge survives — the pull writes back only the runs it
actually changed.

The one thing that is genuinely exclusive is a second merge, because a pull
mutates the shared live checkout. It refuses immediately rather than blocking:

```
weave: a merge is already in progress in this repo (pull.lock held) — retry when it finishes
```

Retry once the first pull reports. If a crashed process ever leaves a lock
behind, the wait is bounded (`queue busy … retry`), not indefinite.

### Tool crashed mid-work

Wrap detected the exit. Workspace is intact for 30 minutes idle grace. Re-attach:

```bash
bashy weave start --resume --issue 124 -- opencode
```

The same workspace, same branch, same author identity. Whatever the tool had committed locally is still there.

### You want to take over manually

```bash
bashy weave shell 124
```

Drops into a shell inside the workspace with the lease's author identity already configured in `git config user.email`. Edit, commit, submit:

```bash
$ vim ...
$ git add -p
$ git commit -m "rebalance under load"
$ bashy weave submit 124
$ exit                            # back to your normal shell
```

### One agent is stuck / hallucinating

Kill it cleanly:

```bash
bashy weave abandon 124 --reason "going to redo with different prompt"
```

Workspace removed, branch removed (since no PR open), lease closed, issue label flips to `loom:abandoned`. Start fresh:

```bash
bashy weave start --issue 124 -- codex      # try a different tool
```

### Safe provider-failover sequence & route quotas (run #244 pattern)

When an agent hits provider rate limits, 429s, API overload, or route exhaustion during execution:

1. **Targeted kill**: Execute a targeted kill of the worker process tree (`weave kill <issue>`) rather than wiping workspace state or abandoning committed work.
2. **Retained workspace/branch/commit**: The workspace, git branch, and any committed work (`commits_ahead > 0`, `salvageable: true`) remain preserved on disk.
3. **Guarded salvage**: Inspect the retained commit, then run `weave salvage <issue>` (or allow automated salvage evaluation); configured deterministic gates are honored, while absent gates do not block the merge.
4. **Deterministic continuation**: If salvage cannot proceed, create a continuation run that fetches the retained commit directly from the retained branch/workspace into the new context, ensuring the campaign advances without re-executing already-completed work.
5. **Route quota separation (Native AGY Gemini vs AGY Third-Party)**: Native AGY Gemini routes (direct Google Gemini quota) and AGY third-party routes (e.g. Anthropic/OpenAI or proxy routes accessible via AGY or cloudbox gateway) operate under separate, independent rate limits and quota pools. Quota depletion on native AGY Gemini does NOT imply third-party routes are exhausted (or vice versa); failover routing must evaluate availability across these independent quota pools.

### You want to merge #125's PR before #124's

By default the merger goes in arrival order. To prioritize:

```bash
bashy weave open --issue 125         # opens the PR in Gitea
# In Gitea, click "Merge now" — bypasses the queue
```

Or via CLI:

```bash
ycode loom merge-now --pr 3          # substrate-admin command
```

### You want to review a PR before it merges

By default the merger auto-merges on green CI. To require manual approval, set in `.ycode/loom.yaml`:

```yaml
merger:
  approval_required: true
```

PRs then wait in `submitted` state until you approve them in Gitea (or via `ycode loom approve --pr N`).

### You want to push intermediate work to GitHub before all three are done

Nothing stops you. After Phase 5.2 (whether one PR or three have merged), `git push origin main` works normally. The local Gitea is just a staging area; GitHub is the truth-source.

### An agent discovers related work while fixing something else

Codex, fixing #123, notices a separate race condition in the eviction path. Inside its workspace, it calls `weave_add` via MCP:

```
weave_add(
  title="race in cache eviction path",
  body="...",
  priority="p1",
  parent_issue=123
)
```

A new issue #126 appears in the queue, labeled `loom:source:agent:codex` and `loom:todo`, linked back to #123 in the comment thread. The next free `weave start` will pick it up (or the human can review and re-prioritize first if `agent_filed_default_state: proposed` is enabled — agent-filed items then carry `loom:proposed` and are excluded from claims until promoted).

### Let an agent rank everything for me

```bash
bashy weave prio --auto
```

LLM reads all `todo` issues, ranks them by impact, and applies `loom:p0`–`loom:p3` labels. You can override any single one afterward:

```bash
bashy weave prio 127 p0   # I disagree, this one's urgent
```

### You closed all three terminals and walked away

Heartbeats stopped. After 30 min idle (default), the reaper:

- Tears down workspaces whose leases have no open PR.
- Leaves workspaces whose leases have an open PR alone (merger may still need them).
- Preserves all branches in Gitea.

When you come back: `bashy weave list` shows what's still active. `bashy weave list --history` shows what was reaped. Lost work surface = zero (everything that was committed in the workspace is preserved as a Gitea branch).

## What you never had to do

- Configure CI for the local Gitea (auto-detected from your project files).
- Set up per-agent identity, tokens, or branch naming conventions.
- Choose a backend mode (auto-selected: `forge` since you had `npm test`).
- Manage workspace lifetimes or TTLs.
- Coordinate the three agents to avoid stepping on each other.
- Resolve a merge conflict in your working tree (the merger handles it in the workspaces).
- Touch the Gitea UI for anything other than glancing at the filtered issue list.

The contract was: three terminals, one command per, walk away, pull, push. Everything else is the substrate's problem.

## Programmatic variant — same workflow, agent-driven

If you're an orchestrator agent instead of a human at a keyboard, the same workflow looks like this (using the agent-friendly CLI conventions from the plan doc):

```bash
export BASHY_AGENTIC=1     # forces --json, no prompts, no colors

# Phase 1 — seed the queue from a planning step (LLM emitted tasks.json)
bashy weave add --from-file tasks.json --json

# (Optional) Re-rank everything via the orchestrator's own LLM strategy.
# Equivalent to a single weave_prioritize MCP call.
bashy weave prio --auto --json

# Phase 2 — spawn N workers. Each weave start atomically claims top-of-queue.
for i in 1 2 3; do
  bashy weave start --tool codex --json &
done
wait

# Phase 3 — watch transitions as NDJSON
bashy weave list --watch --json &
WATCH_PID=$!

# Phase 4 wait — block until everything claimed-and-started is terminal
bashy weave wait --all --timeout 1h --json

# Phase 5 — pull
bashy weave pull --json

kill $WATCH_PID
```

Every command returns a versioned envelope; exit codes distinguish success / config error / state conflict / unhealthy dep. See [Agent-friendly CLI](./loom-v2-plan.md#agent-friendly-cli) in the plan doc for the full contract.

## References

- [`loom-v2-plan.md`](./loom-v2-plan.md) — design rationale for everything in this runbook.
- [`loom.md`](./loom.md) — v1 contract (the substrate beneath `weave`).
- [`backlog.md`](./backlog.md) — task-tracking layer; backlog items can be promoted to weave issues.
