---
name: weave
description: Orchestrate parallel subagent CLIs (codex, claude, opencode, ...) against a queue of issues — isolated workspaces, live steering, recovery, verified convergence
user_invocable: true
---

# /weave — Multi-Agent Orchestration Playbook

Drive `bashy weave` as an orchestrator: file issues, fan out one
subagent CLI per issue in isolated git-clone workspaces, watch and
steer them, recover from kills, and merge verified work back. Every
rule below was earned in dogfooding; the failure it prevents is
named where that helps.

This skill is self-contained: it assumes only the `bashy weave` command
set (available wherever `ycode` is installed) plus whatever worker CLIs
you choose to launch. Run `bashy weave --help` (and `bashy weave
<subcommand> --help`) for the exact subcommands and flags on your machine.

Use this when you have N independent pieces of work for peer agent
CLIs. For one inline edit in the current checkout, just do the work.

## Orchestrator contract

Before filing, launching, judging, merging, or killing anything,
decide which role you can actually hold in your current runtime:

- **Author / scout**: write ready-to-file issues, gates, plans, and
  assignments. Do not launch, merge, or kill.
- **Command**: hold the live session, steer workers, salvage failures,
  judge evidence, and merge verified work.
- **Deputy**: run one fully specified, bounded round, then hand
  control back.
- **Judge / verification**: re-derive evidence and write verdicts.
  Do not edit source, merge, push, or kill.

If unsure, drop one role. A round that fails visibly is cheaper than a
round that silently no-ops.

The queue is the source of truth. Re-read `bashy weave list --json`
before any decision to file, launch, steer, kill, judge, pull, report,
or abandon work. Treat `state`, `exit`, `commits_ahead`, `dirty`,
`verify_exit`, `verify_output`, `verify_tree`, and `killed_by` as
facts. A worker's prose, transcript, or commit message is only a lead
until reproduced. Echo measured numbers exactly from terminal output;
if evidence is absent, write `MISSING`. For improvement work, require
both a passing gate and queue evidence that `commits_ahead > 0`.

Operational invariant: keep five things aligned with observable
evidence — the queue, the workspaces, the workers, the verification
gates, and the merged checkout. If any two disagree, stop and
diagnose before continuing.

## Conductor lock + baton handoff (single-driver — `bashy weave guide` is authoritative)

Two conductors must NEVER drive one campaign at once. Before driving, claim the
lock: `bashy weave baton take --as <you>` — it prints the handoff baton and
REFUSES if another conductor's lock is live (heartbeat < 30m TTL). Writing the
baton heartbeats the lock.

Handoff (like a PM on vacation vs off sick):
- PLANNED: `bashy weave baton write --goal/--stage/--done/--next/--lesson …` at a
  stable point, then `bashy weave baton release`; the successor `take`s it.
- UNEXPECTED (ratelimit/crash): write the baton OFTEN (each stage + before risky
  merges); a crashed holder's lock goes STALE and a successor `take`s it
  (`--force` only if certainly gone), reconciling the baton against live
  `bashy weave list`.

Active supervision: after `weave start`, poll every few minutes and ACT —
STALLED → nudge/reassign; ASKING (permission/clarification) → answer via
`weave say <N> "<answer>"`; ABORTED → reassign/salvage; COMPLETED → verify+merge,
assign next. Update the baton after EVERY action. Run `bashy weave guide` for the
full, always-current playbook.


## Authoring --verify commands

The wrapper runs them with a hermetic `bash --noprofile --norc -c`
in the workspace (10m ceiling). Still: never `bash -l` inside (user
dotfiles), never `set -e` around a `diff` pipeline (exit 1 means
"files differ", not failure), anchor `ROOT=$PWD` before any `cd` into
symlinked corpora, always `echo` the measured number (it lands in
verify_output — the evidence trail), and end with the explicit gate
test (`[ "$n" -lt <baseline> ]`). For improvement work, the merge
decision must also require queue evidence that committed work exists
(`commits_ahead > 0`) so a no-op tree cannot pass. The gate refusing
a merge is `weave pull` reporting verify-failed; the orchestrator
re-runs the gate by hand before overriding anything.

## Quick start — any orchestrator, zero to fleet

You may be Claude Code, codex, gemini, opencode, or anything that
can run a CLI: the flow is identical, and you may enlist YOUR OWN
CLI as a worker too (each worker runs in an isolated clone, so
self-orchestration is safe). From a user goal:

    cd <repo-root>                       # queues are per-repo (cwd-keyed)
    # 0. ORIENT at the target repo root; queues are cwd-keyed.
    # 1. DECOMPOSE the goal into N independent issues with DISJOINT
    #    file scopes; write each body per the Phase 1 contract below.
    #    Complex round? Run the optional Phase 1.5 planning poker.
    bashy weave add "<title>" --priority p0 --body "<body>"   # × N
    # 2. PREPARE (only if the build needs untracked corpora — Phase 2):
    bashy weave start --no-spawn --issue <N> && ln -s <corpus> <workspace>/
    # 3. LAUNCH one worker per issue, backgrounded, watchdogs ON
    #    (tool recipes in Phase 3 — pick per tool, including your own):
    bashy weave start --resume --issue <N> --max-runtime 45m --mem-limit 5g \
        -- <tool> <tool-flags> "<body>" &
    # 4. MONITOR: `weave list` (TOOL/STARTED/DUR), `weave log <N> -f`,
    #    blocked-agent protocol (Phase 4); steer with `weave say`.
    bashy weave wait --all --timeout 50m
    # 5. CONVERGE: verify each claim, then merge + re-measure (Phase 7):
    bashy weave pull <N>                 # targeted; one verified item at a time
    # 6. JUDGE: file one more issue on the merged state for an
    #    independent verification agent (end of Phase 7).

Tool cheat-sheet (details + caveats in Phase 3):

    claude    claude --dangerously-skip-permissions "<body>"        # TUI; pre-seed trust in ~/.claude.json (see Per tool) or say N "1"
    codex     codex exec --skip-git-repo-check --sandbox workspace-write "<body>"   # headless; -s/--sandbox workspace-write (codex 0.141.0; --workspace errors+exits 2)
    gemini    gemini --yolo --skip-trust -i "<body>"                # TUI; no trust dialog
    opencode  opencode run "<body>"                                 # headless; check artifacts, not exit code
    aider     aider --yes-always --no-check-update --message "<body>"  # headless; auto-commits; model from ~/.aider.conf.yml

## Tool report card (update as evidence accumulates)

Reflecting seven dogfood rounds:

- **codex**: reliable workhorse; honest no-ops (declines to commit when its
  change regresses the metric); headless exec exits clean.
- **claude**: strongest on deep multi-file work (delivered both fixture flips);
  TUI needs trust-dialog answer + graceful /exit stop.
- **opencode**: model-dependent — a real dev candidate when paired with a
  strong model, weak otherwise. Always a good verification judge; ingests
  `say` steering while headless; check artifacts not exit codes. ROOT CAUSE
  of its silent exit-0 no-ops confirmed: with no `permission` config it
  REJECTS its own edit/bash tools and exits green — always pre-seed the
  workspace opencode.json (see defense-in-depth matrix). With kimi it no-op'd
  on hard dev work even with permissions fixed. With DeepSeek (capability
  retest) it was night-and-day: ran a full ~35m on a hard gated task,
  decomposed it, explored the source deeply (even spawned an Explore
  Agent), made a regressing change and RECOGNIZED+REVERTED it, wrote
  root-cause fixes (not source-text heuristics), self-ran its gate +
  cross-checked against real bash, and committed clean. BUT the delivered
  change was a NET REGRESSION: it hit its task gate while breaking two
  sibling test files OUTSIDE its guard set — the classic overfit trap,
  caught only by the full-suite sweep. Verdict: promote to first-class
  WORKER (dev + judge); do NOT make it the primary orchestrator — it has
  no long-horizon session track record, its `external_directory` rejection
  fights the out-of-tree workspace traversal an orchestrator needs, and it
  just demonstrated the over-trust-a-narrow-gate failure an orchestrator
  must never have. Fine as a bounded deputy or judge.
- **aider**: passed probation on a gated 3-pointer (sh new-exp
  anchored-substitution fix, −21 diff lines, zero collateral, ~$0.02).
  Reliable surgical edits when the issue body pins exact cases and
  files; auto-commit removes the forgot-to-commit failure mode. Two
  caveats: it resolves test-name collisions by DELETING its own new
  tests (tell it to rename instead — a follow-up resume fixed it on
  the first ask), and it cannot iterate against a test suite in
  --message mode, so the verify gate is the only backstop. Give it
  well-specified one-shot edits, not exploratory work.
- **gemini**: **Promoted to p0 orchestrator.** Strongest on codebase research,
  architectural mapping, and multi-step documentation/context generation
  (the `GEMINI.md` pattern). High success rate on surgical edits and
  complex plan reconciliation. Headless `-p` mode is stable; TUI mode may
  still stall on usage menus — use `weave say N "1"` to clear. Best-in-class
  at "Research -> Strategy -> Execution" lifecycle.


Everything below is the depth behind those six steps — read Phase 1
(issue contract) and Phase 7 (verification) in full before your
first round; skim the rest and return on demand.

## Phase 0 — Before filing anything

- `cd` to the target repo root. Queues are per-repo, keyed by cwd;
  an empty `weave list` elsewhere now hints where the action is.
- Identify anything the task's build/repro needs that is NOT in git
  (vendored corpora, symlinked fixtures, generated assets). Workspace
  clones contain ONLY tracked files — a missing corpus is why one
  dogfood agent escaped its workspace hunting for fixtures and another
  couldn't verify at all.
- In submodule umbrellas, run weave against the actual repo being
  changed, not the umbrella, unless the issue is explicitly about
  umbrella-level docs or pin bumps. Submodule pin bumps require
  explicit human authorization.

## Phase 1 — File issues (the issue-body contract)

One issue per independent task. Every body carries, in order:

1. **Workspace contract** (verbatim-ish): "Your cwd is your isolated
   workspace — a full git clone. Stay inside it; use only relative
   paths; never follow git remotes. Your branch is checked out and
   your git identity is pre-set."
2. **GOAL** — one measurable outcome.
3. **REPRO** — the exact command that measures it, runnable from the
   workspace. If the canonical harness applies filters/env, embed them.
   Anchor binaries BEFORE entering linked corpora: `BIN=$PWD/bin/tool`
   at the repo root, never `realpath ../..` from inside a symlinked
   directory — it resolves to the symlink target's checkout and the
   agent silently measures the wrong build.
4. **SCOPE** — directory allowlist, disjoint from every other issue
   running in parallel (parallel agents must not be able to collide
   even if both succeed).
5. **SUCCESS CRITERIA** — build green, tests green, and: "state the
   EXACT measured number from the repro in your commit message; an
   unverified claim is worse than none." (An agent once claimed a
   25% improvement that did not reproduce.)
6. **Commit discipline** — `git add` files BY NAME; commit to the
   current branch; DO NOT push; DO NOT switch branches; end the
   instructions with `COMMIT YOUR WORK`.
7. **Blockers escape** — "if stuck after 3 serious attempts, commit
   what works plus <TOPIC>-BLOCKERS.md with your diagnosis — include
   a verified patch if the fix lies outside your scope — then exit
   0." (A scoped agent once delivered the complete out-of-scope fix
   this way; the orchestrator applied it. Best outcome available.)
8. **Exit instruction** matched to the tool (see Phase 3): headless
   tools "exit cleanly (exit 0)"; steerable TUI runs "reply DONE and
   wait" (the orchestrator stops them).

## Phase 1.5 — Sprint planning (OPTIONAL — the orchestrator's call)

For complex multi-issue rounds, get second opinions on priority and
effort BEFORE assignment. Skip it for small or obvious rounds; when
used, timebox the whole phase to a few minutes.

1. Compile the issue list (titles + 2-line summaries + proposed
   scopes) into one planning brief with this rubric:
   "Story-point each issue on the Fibonacci scale 1,2,3,5,8 where
   8 ≈ 30 minutes of agent runtime — the hard cap. If you judge an
   issue larger than 8, do not stretch the scale: propose the split
   into smaller issues. Also rank priority (p0-p3) by value and
   unblocking effect. Return STRICT JSON:
   [{id, points, priority, split: [titles...]|null, note}]."
2. Fan the brief out as CHEAP HEADLESS one-shots — these are
   advisory calls, not workspaceed issues (no repo access needed):
       codex exec "<brief>"          claude -p "<brief>"
       gemini -p "<brief>"           opencode run "<brief>"
   All four in parallel; collect within minutes.
3. Reconcile — the orchestrator has FINAL SAY: take the median
   points per issue; any tool-to-tool spread wider than one Fib
   step, or any split proposal, means the issue is under-specified
   — break it down and re-add the pieces. Record the outcome:
       bashy weave point <issue> <1|2|3|5|8>
       bashy weave prio  <issue> <p0..p3>
   (or file with `weave add --points N` directly). PTS shows in
   `weave list`.
4. Assign per the report card and the points. `weave start` derives the hard
   `--max-runtime` from points (1→3m45s, 2→7m30s, 3→11m15s,
   5→18m45s, 8→30m); an explicit value may tighten but never extend it. Send the
   biggest issues to the
   strongest tool first.

## Phase 2 — Allocate and prepare workspaces

When the repro needs untracked content, allocate first, link, then
launch into the prepared workspace:

    bashy weave start --no-spawn --issue N
    ln -s /abs/path/to/corpus  <workspace>/corpus     # whatever Phase 0 found
    bashy weave start --resume --issue N ... -- <tool> "<body>"

Skip this phase when the repo is self-contained.

## Phase 3 — Launch (tool recipes + watchdogs)

Calibrate `--mem-limit` to the TARGET REPO's build profile, not a
global default: a Go repo that links a 500MB binary peaks ~5GB RSS
in the linker alone — a 5g cap killed three builders in one round
(the watchdog forensics named `link` each time). Budget link-peak
plus headroom (12g served).

When verifying agent work via e2e suites that drive a compiled
binary, REBUILD that binary first (make compile / the repo's
equivalent) — judging fresh code against a stale harness binary
produces phantom failures (nearly cost one agent a correct fix).

ALWAYS set watchdogs on unattended runs — they are the reason a
runaway costs a retry instead of the machine:

    --max-runtime 45m --mem-limit 6g     # idle-timeout optional; TUI spinners defeat it

Two watchdog facts: timers count AWAKE time (macOS sleep pauses Go
timers, so DUR can show hours against a 30m cap — not a bug), and
TUI agents don't self-pace toward the deadline the way headless
exec modes do. Give interactive runs a soft budget inside the
prompt ("commit whatever passes by minute 35") or expect to recover
orphaned work after the kill.

## Startup-prompt defense in depth (every tool, every launch)

Interactive tools block on trust/permission prompts; headless ones may
silently no-op instead (worse — exit 0, no artifacts). Apply THREE
levels, in order, for every launch:

1. **CLI flag** — use it when it exists.
2. **Pre-seed config** — when no flag covers it, write the tool's own
   trust/permission cache for the workspace path during prep (the exact
   key the tool's dialog would write).
3. **Monitor + `weave say`** — ALWAYS, even with 1+2: for the first
   ~90s of any TUI launch, poll `weave log N -n 6` for prompt
   signatures — match generic words (`trust`, `approve`, `allow`,
   `Yes/No`, `press Enter`), NOT exact menu text (wording shifts
   between tool versions) — and answer with `weave say N "<answer>"`.

Per-tool matrix (this machine, update as versions change):

| tool | 1) flags | 2) pre-seed | 3) watch for |
|---|---|---|---|
| claude TUI | none for trust (--dangerously-skip-permissions ≠ trust) | `~/.claude.json` → `projects.<workspace>.hasTrustDialogAccepted=true` (+ hasCompletedProjectOnboarding) | "trust" dialog → say "1" |
| codex exec | `--skip-git-repo-check --workspace workspace-write` (no prompts in exec mode) | `~/.codex/config.toml` → `[projects."<workspace>"]\ntrust_level = "trusted"` (needed for TUI mode) | TUI: directory-trust → say "1" |
| gemini | `--yolo --skip-trust` (covers everything) | n/a | usage-limit menus (it stalls there) |
| opencode | NO `--dangerously-skip-permissions` equivalent — config-only; headless `run` silently REJECTS un-permitted tools and exits 0 — the no-op-with-green-exit failure mode | write `opencode.json` into the workspace root at prep: `{"permission":{"edit":"allow","bash":"allow","webfetch":"allow","external_directory":"allow"}}` (never commit it). The `external_directory` key is SEPARATE and gates ANY path outside the project tree (e.g. `/tmp`) even with `bash:allow` — omitting it auto-rejects out-of-workspace reads and causes the no-op; also prefer writing fixtures/diffs INSIDE the workspace | artifacts, not prompts: zero commits + clean exit = rejection happened |
| aider | `--yes-always` (covers all confirmations) | n/a (model/keys via ~/.aider.conf.yml + ~/.env) | reasoning loops ("Already done…" repetition) — kill, don't wait |

Pre-seed snippets (run during Phase 2 prep, $WORKSPACE = absolute path):

    # claude
    python3 - "$WORKSPACE" << 'EOF'
    import json,sys,os
    p=os.path.expanduser('~/.claude.json'); d=json.load(open(p))
    d.setdefault('projects',{}).setdefault(sys.argv[1],{}).update(
        hasTrustDialogAccepted=True, hasCompletedProjectOnboarding=True)
    json.dump(d,open(p,'w'),indent=2)
    EOF
    # codex (TUI mode only; exec mode needs no seed)
    printf '\n[projects."%s"]\ntrust_level = "trusted"\n' "$WORKSPACE" >> ~/.codex/config.toml
    # opencode
    printf '{"permission":{"edit":"allow","bash":"allow","webfetch":"allow","external_directory":"allow"}}' > "$WORKSPACE/opencode.json"

Per tool:

- **codex, headless**: `codex exec --skip-git-repo-check --workspace
  workspace-write "<body>"` — exits cleanly on completion. NOTE:
  `--full-auto` is deprecated and, on current codex, exec aborts with
  "Not inside a trusted directory and --skip-git-repo-check was not
  specified" (exit 1, 0s) — always pass `--skip-git-repo-check`.
- **codex, steerable TUI**: `codex -s workspace-write -a never
  "<body>"` — answers nothing by itself: expect the directory-trust
  prompt and clear it with `weave say N "1"`. Does not exit when
  done (see Phase 7).
- **claude**: NEVER bare `claude -p` for a run you want to watch or
  steer — it buffers ALL output until exit (empty capture, ignores
  injected keystrokes). Use `claude --dangerously-skip-permissions
  --verbose --output-format stream-json -p "<body>"` for a streaming
  headless run, or accept fire-and-forget and read its transcript
  under `~/.claude/projects/<workspace-slug>/` for progress.
- **claude TUI trust dialog**: there is NO flag to pre-trust a folder
  (`--dangerously-skip-permissions` does not cover it; only `-p` mode
  skips trust). PRE-SEED the per-directory cache during workspace prep
  instead — upsert into `~/.claude.json`:
      python3 - "$WORKSPACE" << 'EOF'
      import json,sys,os
      p=os.path.expanduser('~/.claude.json'); d=json.load(open(p))
      d.setdefault('projects',{}).setdefault(sys.argv[1],{}).update(
          hasTrustDialogAccepted=True, hasCompletedProjectOnboarding=True)
      json.dump(d,open(p,'w'),indent=2)
      EOF
  Undocumented but it is exactly the key the dialog itself writes.
  Fallback if the dialog still appears: `weave say N "1"` — and match
  on the word "trust" in the log, not exact menu text (the dialog
  wording varies across claude versions).
- **gemini**: `gemini --yolo --skip-trust -i "<body>"` — interactive
  (steerable) with all tool approvals auto-accepted and the
  workspace trust dialog suppressed; exits on `/quit` (the graceful
  kill handshake covers it via its second verb). Headless
  alternative: `-p "<body>"`.
- **aider**: `aider --yes-always --no-check-update --message "<body>"
  [files...]` — headless one-shot, exits clean, and AUTO-COMMITS its
  own edits (no orphan-commit rescues). Model comes from
  ~/.aider.conf.yml (this machine: DeepSeek-only by owner directive —
  do NOT pass --model); aider brings its own DEEPSEEK_API_KEY — weave
  never injects a provider credential (it is not weave's job to hand
  out API keys; every agent is preconfigured with its own auth). If
  DEEPSEEK_API_KEY lives in the operator's own environment, weave's
  default credential-shape scrub still strips it before launch like
  any other `*_API_KEY` name — set `BASHY_ALLOW_AGENT_SECRETS=1` for
  that run, or preconfigure the key outside weave's env entirely.
  Litter caveat: it creates/edits .gitignore and .aider* files in the
  workspace — never stage those. Interactive mode exits on /exit.
- **opencode**: `opencode run "<body>"` — streams live, exits clean,
  and DOES ingest `weave say` lines mid-run (steerable while
  headless). Two caveats: its own permission system auto-rejects ANY
  path outside the project tree — file-tool reads through symlinks AND
  bash tool calls touching e.g. `/tmp` (the `external_directory`
  permission, separate from `bash`; log line `permission requested:
  external_directory (...); auto-rejecting`). Grant
  `"external_directory":"allow"` in opencode.json AND write
  fixtures/diffs INSIDE the workspace, not `/tmp` (a `/tmp`-resident diff
  is exactly what no-op'd it once) — and a rejected tool call can end
  the run with EXIT 0 and no deliverable: always check for the artifact,
  never trust the exit code alone.
  CONTAINMENT WARNING: opencode keeps persistent per-project state;
  if it has ever worked in the origin repo it gravitates back to it
  by absolute path — even with the workspace's `origin` remote removed
  (observed twice: committed to the real checkout's master, workspace
  branch left empty). Prefer codex/claude when workspace containment
  matters; if you do use opencode, check `git -C <workspace> log` AND
  the origin repo's HEAD at completion before trusting state.

Background each start (`&`); the wrapper auto-setsids.

## Phase 4 — Monitor

    bashy weave list             # TOOL + STARTED + DUR: who works what, since when
    bashy weave log N -f         # live PTY capture (any number of watchers)
    bashy weave log N --summary  # compact outcome: state/exit/verify/commits/merged
    bashy weave status N         # where one issue stands + merged-into-main yes/no
    bashy weave list --watch --json   # NDJSON state transitions

`weave list` reconciles state against git: a `submitted` item whose
commits already landed in the base branch (merged out-of-band, not via
`weave pull`) shows as `done` and is swept by `weave prune` — no more
stranded items that only `abandon` could clear. A footer flags terminal
items still holding workspace clones on disk (`weave prune` to reclaim).

Never measure benchmarks/suites on the host while subagents compile
in parallel — per-test timeouts flake under load and read as
regressions.

EARLY-STOP / PARTIAL-COMPLETION PROTOCOL — proactively monitor; a
worker reaching `submitted` (or a headless tool exiting) does NOT mean
the assignment is done. Agents routinely stop after fixing only PART of
the goal — codex exec especially submits after one or two easy clusters,
often with the work UNCOMMITTED. Judge against the GOAL, not the state:
on each poll, and always before accepting an outcome, RE-MEASURE the
target metric in the worker's workspace — never trust "submitted". If the
goal isn't met (target not at 0, only a partial reduction, failures
remain), the worker stopped early — RE-DRIVE it, don't accept the
partial:

    bashy weave start --resume --issue N ... -- <tool> "<harder prompt>"

The harder prompt must (a) state exactly how far it got vs the goal
("you're at 52, only fixed 2; your work is also uncommitted"), (b) make
the loop EXPLICIT and iterative — measure → read EVERY remaining failure
→ fix the next cluster → gate (full suite + unit tests) → COMMIT → repeat
— and (c) forbid stopping until the metric hits the goal or each
remaining item is documented in a BLOCKERS note with a concrete reason.
Reinforce mid-run with `weave say N "keep going; commit each cluster"`.
Resume as many rounds as needed: one partial submit is a checkpoint, not
the deliverable. (Pairs with the dirty-workspace / "submitted ≠ committed"
check in Phase 7 — salvage and commit the tree before re-driving, so the
partial progress isn't lost when you resume.)

BLOCKED-AGENT PROTOCOL — TUI agents stall on dialogs, and a status
question typed into a menu is noise. Watch for waiting-screens in
the capture tail (selection menus `● 1.`/`❯ 1.`, "Enter to
confirm", y/n, `[y]`-style prompts, "Press enter", usage/rate
limits, auth prompts, idle composer). Screen text alone can be
SCROLLBACK — a rendered pane from a command that already finished
(one orchestrator answered a `patch … Assume -R? [y]` prompt that
had been dead for minutes, typing noise into the composer). VERIFY
the block is live before answering:

    ps -axo pid,ppid,stat,command | awk -v wp=<wrapper_pid> '$2==wp'
    # then walk children: a LIVE block shows a child process
    # (patch, a shell, an editor) sitting in a foreground TTY wait;
    # get wrapper_pid from `weave list --json`.

Live block confirmed — or a top-level dialog with NO spinner and no
running children (trust dialogs, limit menus) — then respond to
WHAT THE SCREEN ASKS:

- Orchestrator handles automatically: workspace-trust dialogs
  (`say N "1"`), model-switch/limit menus where one option clearly
  continues the task, composer nudges (Tab to queue, Enter), any
  confirmation consistent with the issue body. (A gemini run once
  sat 30 minutes on a "limit reached — switch model?" menu; the
  answer was `say N "1"`.)
- Escalate to the human, never auto-answer: authentication/login,
  API keys, billing/upgrade decisions, anything destructive or
  outside the issue's scope, and any dialog you can't classify.
  Surface the captured lines verbatim when asking.

## Phase 5 — Steer (weave say)

    bashy weave say N "btw, status check: one line — current measured
    number and what you're working on — then continue."

- One say = one typed line + Enter. Injected text is keystrokes; the
  agent treats it as user input.
- Text injected while nothing reads it lands in the composer and is
  delivered as the NEXT user message — harmless; instruct workers to
  ignore stray one-liners.
- codex mid-turn QUEUES typed input on Tab instead of submitting:
  follow the say with a literal Tab via the control socket
  (`printf '\t\n' | nc -U <ctl_sock>`). Avoid a leading `/` on plain
  messages — codex parses it as a command-palette trigger.
- One stdin writer at a time. Two writers interleave in the composer
  (it happened); coordinate before injecting.

## Phase 6 — Recover

- `bashy weave doctor` is the one-shot recovery pass: it runs the lifecycle
  REAPER and then lists every open run with the step that closes it. The reaper
  also runs on every `weave list` / `weave status` / heartbeat tick, so you
  rarely need it — but it is where "why is this run stuck?" gets an answer
  instead of a shrug. It never removes a workspace, branch, or commit; disposal
  stays `weave prune` / `weave abandon`. See
  `docs/weave-lifecycle-state-machine.md` for the whole state machine.
- Wrapper died / machine restarted: the reaper transitions the run to
  `failed` (`wrapper-died`) instead of leaving it "working" forever →
  `weave start --resume --issue N -- <tool> "<follow-up body>"`.
  Resume works from any state with a preserved workspace.
- A run marked `failed`/`killed` that is flagged **salvageable** has committed
  work on its branch — measured from git, not from the record the dead wrapper
  never wrote. Inspect it, then `weave salvage N`; do NOT re-run it. Salvage
  shows the diff, honors any configured deterministic gates, and never pushes.
  Add `--review-agent <agent>` only when model review is explicitly wanted.
- A `submitted` run flagged **needs-steward** has been sitting unmerged past the
  threshold; the flag names the decision owed (pull, reverify, or abandon). The
  reaper deliberately does not merge for you — `weave pull` owns the verify /
  review / isolation gates.
- Watchdog killed mid-work: the workspace survives. Inspect
  uncommitted changes — they are often real progress (one killed run
  held a 3x diff reduction uncommitted). Verify, commit them as the
  orchestrator, then resume with a short follow-up prompt listing
  only what remains.
- Agent finished but claimed results you can't see: NEVER trust the
  claim — re-run the issue's REPRO yourself before merging. Park
  unverifiable work on a branch; re-file with a hardened prompt.
- **WARNING**: resumed workspaces keep their ORIGINAL base commit — work
  resumed after sibling merges lands on a stale base; rebase in the workspace
  or expect conflicts at pull.

## Phase 7 — Converge and verify

    bashy weave wait --all --timeout 50m
    bashy weave list --json               # read state/exit/commits/dirty/verify fields
    bashy weave pull N                    # targeted: one clean, committed, verified item

- **Check for a dirty workspace BEFORE pull**: `git -C <workspace> status
  --porcelain` (ignore `.aider*`/`GEMINI.md` litter). The wrapper's
  verify runs against the WORKING TREE, but pull merges only COMMITS —
  an agent that commits an interim state and keeps improving
  uncommitted gets a green gate attesting work that never merges, and
  workspace cleanup destroys it (this shipped a silently-regressed
  master once: gate said redir=0, merged commit measured redir=4).
  If dirty with real changes: resume the agent with "commit your
  work", or commit it yourself in the workspace, and re-verify.

- **Enforce the cap — split, don't extend.** 8 points = 30 min, enforced by
  `weave start`. Near the cap with the core deliverable already committed,
  wrap it (`say` a commit-and-exit order, then graceful
  `weave kill`) and file the residue as FRESH, smaller issues — an
  agent polishing past its cap delivers less than a new scoped run.
  Better: don't file open-ended 8-pointers at all; scope each issue
  to one named cluster with its own gate so progress is verifiable
  in 5-point bites.
- Steerable TUI runs don't exit on DONE. `weave kill N` now tries a
  graceful `/exit`//`/quit` handshake first — a tool that obeys
  exits 0 and the wrapper records a genuine `submitted` (then
  `pull` merges it normally). Verify first when in doubt:
  `weave say N "btw, are you done? reply DONE"` and watch the log.
  Only a non-responding tool gets force-killed, recorded as
  `killed` (its own state — never promoted) with wrapper-measured
  evidence (`commits_ahead`, `head`) on the item. If you are not in
  the command role, record the state and leave the workspace intact.
  If you are in the command role, inspect, re-verify, commit any real
  residue in the workspace, and prefer `bashy weave pull N` once the
  item is clean and attested. Manual fetch/merge is salvage of last
  resort, not normal convergence, and must be reported as salvage.
- After pull: rebuild, run the canonical measurement, then run the
  FULL suite on a QUIET machine — not just the fixtures the agents
  worked on. Cross-fixture ripple is real: one round improved its
  target fixture while silently flipping an unrelated one from PASS
  to FAIL by deleting a quirk-encoding block it didn't need to
  touch. Bisect any regression against the pre-merge commit before
  accepting the round.
- Escaped commit (work landed on the ORIGIN repo's branch instead
  of the workspace): don't reflex-revert. Verify it canonically in
  place — exact repro, full package tests, inspect any test
  deletions for legitimacy. Keep it if real (record via
  `weave abandon N --reason`), reset and park on a branch if not.
- Clean residue: `weave abandon N --reason "<why, where the work
  went>"` — the reason is the audit trail.

- Hard stop: pushing, destructive cleanup, and submodule pin bumps
  require explicit per-action human authorization.

For multi-issue rounds, finish with a JUDGE pass (the runbook's
Pattern B): after merging, file one more issue on a fresh workspace
off the merged state — independently re-measure every claim, run
the full suite, audit each commit for test deletions/scope spills,
and commit a verdict report. A judge round caught nothing the
orchestrator missed exactly once so far; every other time it
surfaced a reconciliation worth recording.

## Phase 8 — Close: zero residue (MANDATORY — Sprint 224)

The sprint is not over when the last merge lands. It is over when every run
has a recorded DISPOSITION, every byte the sprint owned is gone, and `sprint
end` says `residual: 0`. Do this while you still hold the context — a later
operator would have to reconstruct story ownership and merge history before
daring to delete anything, which is exactly the review work cleanup exists to
do once.

    # 1. accept what passed — pull records disposition=merged and reclaims the run
    bashy weave pull N
    # 2. decide about everything else — a disposition implies preservation:
    #    any unmerged tip goes to refs/salvage/abandoned-N BEFORE teardown
    bashy weave abandon N --disposition rejected   --reason "gate red twice"
    bashy weave abandon N --disposition superseded --reason "landed via #M"
    bashy weave abandon N --disposition empty      --reason "no diff"
    # 3. end — refuses any undecided run (and names the verb), reclaims what the
    #    settled runs own, looks again, closes only at residual 0
    bashy sprint end <sprint>
    #    → "… reclaimed 3 workspace (…bytes), 3 cache, 2 branch; residual: 0"

- Do NOT report completion unless the `sprint end` line carries
  `residual: 0`. A refusal names the exact run and the exact verb; run it, then
  `sprint end` again. There is no `--force` on this path, on purpose.
- What `end` reclaims is only what the sprint's runs OWN: workspace, managed
  Go cache, per-run ycode agent data, log, socket, lifecycle lock, and the two
  branch names `agent/weave-issue-N` / `agent/weave-issue-N-reviewed` — each
  under proof (no worktree, every patch upstream or under the salvage ref,
  tip unchanged). Repo-wide branches and worktrees are reported, never
  touched.
- `bashy sprint prune <sprint>` is the same question asked read-only at any
  time; `bashy weave gc` asks it for every queue on the machine (renamed or
  deleted repositories included) and `--apply` acts. Both refuse on doubt and
  say why by path.
- A run's row survives teardown compacted: id, title, state, disposition,
  reason, head, salvage ref. `weave list --history` still shows it. The
  workspace, log and socket paths are cleared because the bytes are gone.

## Shared session (HITL-v2) — remote participants, directives, handoff

OPTIONAL layer. When the work is bound to a cloudbox **Task** (a shared
"working session"), other humans/tools on other hosts can join to observe,
inject info, steer, or take over as orchestrator. Local single-host
orchestration (Phases 0–7) is unchanged; this adds a remote control plane.
The session is a cloudbox Task + its append-only event log; the
orchestrator-of-record is the Task's drive **lease** holder.

**Verbs** (all over the joined session; `weave join` writes a session
pointer in the repo's queue state so the rest need no task id):

- `weave sessions` — list joinable sessions (owned + shared-with-me).
- `weave join [<task-id>]` — no arg = the current session; prints the
  continuity record (summary + history) and tails the live feed.
- `weave note "<text>"` — append a `note` (new info for the fleet).
- `weave steer <run> "<text>"` — append a `directive` `{run, verb:"say",
  arg}` aimed at the agent on issue `<run>` (the remote analogue of the
  local `weave say`).
- `weave take [--as <tool@host>] [--force]` — claim the orchestrator lease
  (become the driver); `--force` preempts a dead/throttled holder.
- `weave handoff --to <tool@host>` — graceful: checkpoint the summary,
  release the lease; the successor runs `weave take --as <that>`.
- `weave roster` — who's joined + the current lease holder.

**Conductor duty — consume directives.** When you hold the session, run

    weave conduct [--interval 3s]

ALONGSIDE `weave start` (a second long-lived process). It polls the
session feed for `directive`/`note` events since a persisted cursor
(`<queueDir>/directive-cursor`) and applies each directive to the LOCAL
queue via the same primitives as `weave say`/`add`/`prio`/`kill`, then
appends an `ack`. So a remote `weave steer 3 "use the v2 endpoint"` lands
as a live `say` into the agent on issue #3, and an `ack` shows up in the
feed on every host. Unknown verbs are ack'd `unknown_verb` and skipped
(the loop never aborts); `note` events are informational (skipped, cursor
advances). The cursor is the idempotency guard — a restarted `conduct`
resumes without re-applying.

**Handoff + preemption (the three modes).**
- *Graceful*: holder `weave handoff --to X` (checkpoints + releases);
  successor `weave take --as X` resumes from the continuity record — no
  in-flight state needed, the log IS the state.
- *Forced*: `weave take --force` preempts a holder that can't release
  (throttled/dead). This bumps the session's **fencing epoch**.
- *Tolerate being preempted*: if you were force-claimed, your writes start
  failing with a stale-epoch conflict (HTTP 409 `stale_epoch`). That is
  NOT a transient error to retry — it means a takeover replaced you. Stop
  driving; you have been handed off. (This is what makes automatic
  failover safe: a resumed old orchestrator cannot double-drive.)

## Tool-level failover (HITL-v2 E2) — switch the tool, not the model

Subscription-billed CLIs (claude/codex Pro/Max) hit rolling-window + weekly
caps that emit NO clean HTTP 429 — they print a usage-limit message and
exit. The cloudbox gateway's 429-aware failover (E1) CANNOT see this (no
429 on the wire), so the **conductor** must detect it and switch the whole
TOOL, not just the model.

**The signal.** When a worker terminates, weave classifies its PTY-log tail
and records two fields on the item (visible in `weave list --json` /
`weave log`): `throttled: true` + `throttle_signal: "<matched phrase>"`
(e.g. "usage limit", "rate limit", "429", "weekly limit"). A bare non-zero
exit with no throttle phrase is a CRASH, not a throttle — it is NOT marked
(so an ordinary failure doesn't waste a tool switch).

**The move.** On a `throttled` item, re-launch the SAME issue with the NEXT
tool in the fallback order, using that tool's Phase-3 recipe:

    claude → codex → opencode → aider → local (ollama)

    weave start --issue N -- <next-tool> <recipe...>

Record the switch (`weave note "issue N: claude throttled (<signal>) →
codex"`, or a session `directive` if bound to a shared session). Walk the
order until a tool completes or the list is exhausted; **local Ollama is
the un-throttleable floor** — it never hits a subscription cap, so the task
always has somewhere to land. Don't re-pick a tool already throttled this
round. The throttle is per-rolling-window, so a tool that throttled may
recover later — fine to put it back at the end of the next round.

This is the host-side complement to E1: E1 fails over MODELS/backends inside
the gateway on a clean 429; E2 fails over TOOLS in the conductor when the
throttle is invisible to the gateway.

### Tool throttling & cooldown (automatic re-engage)

Weave now closes the loop so you don't babysit the reset:

1. **Fail over NOW (E2).** On a `throttled` item, re-launch the SAME issue
   with the next *available* fleet tool (the move above). Query availability
   with **`weave fleet`** (or `weave fleet --json`) — it lists each tool as
   `available` or `cooling until HH:MM`, so you skip a tool that is still
   capped instead of bouncing off it again.
2. **Weave records the cooldown.** When a run terminates throttled and the
   message carries a parseable reset ("try again at 11:41 AM", "in 5
   minutes", "Retry-After: N"), `weave start` writes the tool's available-at
   to a per-queue cooldown store (best-effort — it never fails the run).
   `weave fleet` reads it. Limitation: when the tool was launched via a
   `bash -c '<tool> …'` wrapper, weave best-effort recovers the inner tool
   name from the throttle log; if it can't, the cooldown is keyed coarsely
   under `bash` (still nudges a switch).
3. **Re-engage automatically.** The throttle is per-rolling-window, so once
   `weave fleet` shows the tool `available` again, put it back in rotation —
   no manual timer. Local Ollama remains the un-throttleable floor.

So the conductor loop is: `weave fleet` → assign an available tool → on
throttle, fail over + let weave record the cooldown → re-check `weave fleet`
next round and re-engage the recovered tool.

### Safe provider-failover sequence & route quotas (run #244 pattern)

When a run encounters provider-level rate limits, 429s, or route exhaustion, follow the safe provider-failover sequence (demonstrated by run #244):

1. **Targeted kill**: Perform a targeted kill of the affected worker process tree when a provider failover trigger or throttle occurs, leaving the workspace intact on disk.
2. **Retained workspace/branch/commit**: Ensure the run workspace directory, git branch, and any committed work (`commits_ahead > 0`, `salvageable: true`) remain intact on disk. The reaper and watchdog surface work on killed/failed runs rather than destroying it.
3. **Guarded salvage**: Inspect the retained commit, then execute `weave salvage <issue>` (or let salvage evaluation run); configured deterministic gates are honored, while absent gates do not block the merge.
4. **Deterministic continuation**: If salvage cannot proceed, create a continuation run that fetches the retained commit directly from the retained branch/workspace into the new continuation context. This guarantees progress without re-executing already-completed work or stalling in limbo.
5. **Route quota distinction (Native AGY Gemini vs AGY Third-Party)**: Native AGY Gemini routes (direct Google Gemini quota) and AGY third-party routes (e.g. Anthropic/OpenAI or proxy routes accessible via AGY or cloudbox gateway) operate under distinct, independent rate limits and quota allocations. A 429 or quota limit on native AGY Gemini does NOT mean AGY third-party routes are exhausted (or vice versa); failover selection must evaluate availability across these separate quota pools rather than assuming a global outage across distinct route types.

## Conformance / fidelity campaign patterns (learned 2026-06-24, bash-5.3 drive 86%→90%)

- **Gate around a RED base.** If the target repo's full suite is red on `main` for
  ENVIRONMENT reasons (sh's `TestParseConfirm`/`TestRunnerRunConfirm` shell out to the host's
  bash 3.2 on macOS), a round gated on the bare suite false-FAILS good work. Set `--verify` to
  skip them (`go test ./... -skip 'TestParseConfirm|TestRunnerRunConfirm'`); judge a round by
  NEW failures vs the base, never absolute green.
- **Specificity drives yield.** A generic "make X match bash" closes ~1 case/round; embedding
  the EXACT per-case `expected-vs-actual` DIFFs closes ~3×. Paste the concrete failing cases
  (binary-safe extraction — probe output has multibyte/control chars, use python not awk/grep
  -o), point the worker at the on-disk case scripts, and give it the oracle command to
  self-verify.
- **Decompose STRUCTURAL clusters by root cause — don't spray cases.** When a big cluster
  resists even a sharp per-case round (closes ~2 of N), it's not the spec — it's several
  distinct ROOT gaps in one bucket. Do a one-pass investigation (group failures by signature),
  then weave ONE round per root cause with that hypothesis. (array: one-blob round −2/35;
  decomposed into sparse / unset / arith-subscript / shopt → −17.)
- **Cross-repo measurement.** Fix in an engine repo, metric downstream (sh fix, bashy probe):
  gate the round on the ENGINE's own unit tests; the ORCHESTRATOR measures the downstream
  metric post-merge (rebuild downstream + run the probe). Give the worker a self-verify path
  (build the engine's standalone binary + diff vs the oracle) so it isn't fixing blind.
- **Entangled clusters: parallel execution, SEQUENTIAL gated merge.** Rounds that share a
  source file run concurrently but merge one at a time, full-suite re-gate each, conflicts
  resolved by COMBINING fix-sets — then re-run ALL touched clusters' tests (a clean textual
  merge can silently regress one cluster; this caught a var-op rewrite that broke assign's
  dynamic-subscript case). "Just weave it" = `weave add` + `weave start`: queue the issue,
  enlist an idle tool.

## Failure modes, condensed

| Symptom | Likely cause | Move |
|---|---|---|
| `weave list` empty | wrong cwd | follow the hint; cd to the repo |
| capture empty, agent "working" | tool buffers (claude -p) | read its transcript; next time use streaming mode |
| agent cites paths outside workspace | corpus missing in clone, or remote followed | Phase 2 links; origin is removed from workspaces — verify `git remote` is empty |
| claimed improvement won't reproduce | agent measured differently | re-run the issue's exact REPRO; park the branch |
| state=failed after a kill you issued | kill bookkeeping | branch is intact; inspect, re-verify, then prefer targeted `weave pull N`; manual fetch/merge is command-role salvage only |
| exit 143, no commits | watchdog kill | check workspace for uncommitted progress before re-filing |
