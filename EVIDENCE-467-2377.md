# Evidence — Sprint 467 Story #2377 (c049c44e3c71) — amended per manager review 02:16

agy workers under weave stall in discovery instead of using the given paths.

## Verdict (amended)

No harness defect found in retained evidence. No source change. This is
NOT a three-case proof: the issue-223 raw log is lost (global GC; disclosure
in the story is canonical), so cwd/prompt for 223 are explicitly UNKNOWN.
Findings below cover only what the two retained logs show, plus the 223
survivor result.md. Original commit 0faca8a5 is preserved; this is an
evidence-only amendment, no source/test rerun (per 02:16 review).

## Launch path (unchanged from 0faca8a5, code refs only)

Launch path: `weaveExpandAgent` (`pkg/weave/weave_agent.go:152`) sends
`body + weaveWorkerContract` verbatim as the final argv element;
`weaveChildEnv` (`pkg/weave/weave_agent.go:270`) pins `PWD` to the allocated
workspace and drops inherited `PWD`/`OLDPWD`; the agy binding adds
`--new-project --add-dir {workspace}` plus a plan-mode PWD preflight
(`pkg/fleet/baseline/tools/agy.yaml:109-113`). No filesystem sandbox exists
for agy workers by design (only `max_runtime`/`mem_limit`/`idle_timeout`
watchdogs); workers need full user privileges for builds and git.

## Case 1 — Sprint 446 run 414 (S8, agy, ~5m terminal no-op, zero diff/commit)

Retained logs (byte-identical copies, no HOME discovery performed):
`<WEAVE-YOKE-ROOT>/logs/issue-414.log` (49 lines) and
`<EVIDENCE-DIR>/issue-414.log`.

- Initial cwd correct: `init.cwd` = `<WORKSPACE-414>` (line 2).
- Initial prompt delivered verbatim (queue record: issue body + contract,
  with repo-relative spec refs `docs/todo/e4953e9f9ca7-...md` and
  `docs/sprint-446-master-execution-plan.md S8`). Those paths live in the
  umbrella repo, not the yoke workspace, so the first `view_file` failed
  `ENOENT` (line 6) — a manager-authored cross-repo ref, not harness
  rewriting (weave must not rewrite briefs).
- Workspace-scoped probing followed: `git status && ls -la docs/`,
  `ls docs/todo/*...* docs/*sprint-446*` (no match), `find . -name ...`
  (0.33s, empty), `bashy sprint show 446`, failed `bashy story/todo`
  lookups, `bashy sprint --help`.
- CHRONOLOGY CORRECTION (02:16 review): the HOME-wide
  `find <HOME> -name "sprint-446-master-execution-plan.md"` went ACTIVE at
  line 44 BEFORE the line-45 steering echo. Line 45
  ("Story spec is in umbrella <UMBRELLA>/docs/todo/e4953e9f9ca7-...md ...")
  is a raw non-JSON terminal echo of the conductor's 08:53:38Z comment, NOT
  a model acknowledgment. The model's terminal text is generic
  ("locating ... across the workspace and system paths") with no ACK of the
  exact path. The find ran 236.5s and ended `context canceled` (line 47);
  its partial output lists 3 hits including the umbrella plan path, but the
  run still ended no-op (exit 0, no diff). So: the model did NOT know the
  exact path before starting the HOME scan; late steering arrived mid-find.
- Consumption of late steering is NOT assumed: per s467-manager, the MB
  Mira-2 route lacks a read cursor, so delivery-to-terminal is not proof the
  model consumed it. Distinguish correct initial prompt/cwd (held) from
  late steering (echoed mid-find, consumption unevidenced).
- Harness observed and classified it: queue `outside_workspace_paths`
  includes `<HOME>`; run state `no-op`, exit 0, no diff. That telemetry is
  correct classification, not a prompt-delivery failure — do not infer a
  harness prompt bug solely from the model's HOME scan (per mb:6344).

## Case 2 — Sprint 467 run 397 attempt 2 (agy, ~18 min, no diff)

Retained log: `<TMP>/s467-run397-attempt2.log` (139 lines).

- Initial cwd correct: `init.cwd` = `<WORKSPACE-397>` (line 2).
- The worker WAS given and successfully read the absolute spec path
  (`view_file` on `<UMBRELLA>/docs/todo/f0ca0d513925-...md`, 17 lines),
  plus `CLAUDE.md`.
- Discovery still followed, but scoped to the umbrella repo
  (`find <UMBRELLA> -maxdepth 3 ...`; `ls -la <UMBRELLA>/docs/`, 746-line
  listing) — NO HOME scan in this log. Time also went to
  `bashy kb search` + `inbox` + `todo list` in one shell (49.75s) and long
  `sprint --help`/`todo --help` reads. "Steering ignored" belongs to the
  separate open story e6746b00, not this one.

## Case 3 — Sprint 382 run 223 (8 min, zero changes) — RAW LOST

Raw log lost to the 2026-10-18 global `bashy weave gc --apply` (story's
evidence-loss disclosure is canonical; not reconstructed, no recovery
attempted). Survivor: `<WEAVE-BASHY-ROOT>/logs/issue-223.result.md`
(12 lines): repeated find/grep hint noise, then `/exit`, `/quit`,
`error: context canceled`. The model never engaged the task.cwd, prompt
delivery, and sandbox for 223 are UNKNOWN — no harness finding is claimed
for this case.

## Scoped smoke (preserved from 0faca8a5, no rerun per 02:16)

`go test ./pkg/weave/ -run
'TestExpandAgentSelectsTheModel|TestExpandedAGYKeepsPromptFlagAdjacentToPrompt|TestExpandAGYCarriesGenericWorkspacePlaceholder|TestExpandedBodyIsTheFinalArgument|TestWorkspacePreflightVerificationFailsClosed|TestSeededContractArgsUnchanged|TestSeededContractCarriesTheLaunchQuirks|TestEmptyBodyFallsBackToTheTitle|TestMultiTokenArgvIsVerbatim'
-count=1` — ok (1.5s, recorded in 0faca8a5). Launch-contract behavior pinned
by existing tests; no code changed, so no vet/crossvet delta. No heavy
suites on Dragon, no global gc, no other run touched.

## Scope notes

- No edit to `pkg/weave/weave_impl.go` or any source: nothing to coordinate
  beyond notifying s467-manager (claim receipt + this submit). Prompt
  toughening or filesystem confinement would be a feature with cross-repo
  side effects (run 414's spec legitimately lived outside its workspace),
  not a minimal bug fix.
- Manager-actionable (not this story): briefs should carry absolute spec
  paths when the spec lives outside the worker's repo; the 414 relative
  refs cost one failed read before the model chose the worst fallback.
  Live-steering consumption is tracked under story e6746b00.
