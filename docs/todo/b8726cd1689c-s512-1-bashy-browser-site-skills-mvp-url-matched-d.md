---
id: b8726cd1689c
kind: task
title: 'S512.1 bashy browser site skills MVP: URL-matched dhnt skills (browser skills lookup + extract hint + one seed site skill proven live)'
seq: 2820
status: done
priority: p1
labels:
    - browser
created: 2026-10-28T18:19:59.801925Z
weave: 655
assignee: codex-gpt6-astra
sprint: 512
sprint_id: 9f31a532-c25d-5504-8bd8-163f39bd871d
sprint_title: 'bashy browser site skills: the Bash# skill/script duality applied to web pages (thin extension core + per-site skills, self-healing)'
closed: 2026-10-28T20:19:53.461057Z
resolution: fixed
closed_by: codex-gpt6-astra-e
---

Per docs/sprint-512-browser-site-skills.md (umbrella). A site skill is an ordinary dhnt Bash# skill (SKILL.md, metadata.browser-match URL patterns, fenced bsh steps calling bashy browser --mode live, @guard cap, @ensure page-state contract, human-only steps via bashy ask). 'bashy browser skills' lists skills matching the driven tab; extract prints a one-line hint. Self-healing = existing 'skill run --adapt' + local PR gated by fixture pages. Seed skill: one real form proven live.

DONE by the manager (do not redo): yoke 01f1f194 (browser skills + extract hint, lazy SkillOptions), bashy e69a07cc (agentos wiring), umbrella skills/typeform-fill (99eaf523e, e58653682), live fixture proof on ext 0.8.1. The operator moved the real-Typeform run out of this sprint.

== WORKER BRIEF (operator 2026-10-28: fix inside this story) ==
BUG: `bashy skill run go-repo-health` -> "has no skill.dhnt — a prose-only skill is read by a model, not machine-run". yoke pkg/skills/run.go prepareRun requires skill.dhnt, but Sprint 458 M3c turned the umbrella skills (skills/go-repo-health, skills/resume-a-killed-run, skills/unblock-outpost-promotion, skills/typeform-fill) into single text-outer Bash# units with NO skill.dhnt: the contract, cap and steps are the decorators (@require/@ensure/@guard/@effects) on the ```bsh fence in SKILL.md. So no Bash# skill can `skill run` or `skill run --adapt`.

FIX (keep the skill.dhnt path working unchanged for skills that still have one):
1. `skill run NAME`: when the skill has no skill.dhnt and SKILL.md has one or more ```bsh (or ```bashsharp) fences, concatenate them in order and execute them as Bash# in the caller's cwd with the caller's env, through bashy's own Bash# interpreter, so @require/@ensure/@guard/@effects are enforced by the runtime (they already are: `bashy FILE.bsh` returns exit 3 with "postcondition failed" on an @ensure miss). Exit 0 iff the body exited 0. Stream command output to stderr; the receipt goes to stdout (and --json), like today's run.
2. Attest every run into the same host-local attestation store as today (AppendAttest / AttestRecord): skill name, identity = sha256 of the executed fence bytes, the @guard effects cap, exit code, context key/tier as prepareRun computes them. Don't fork the dhnt Attestation type; add what is needed for a Bash# unit receipt additively.
3. `skill run --adapt NAME` on a failing Bash# unit: give the repair agent (--repair-agent, same mechanism as adapt.go) the SKILL.md and the failure output; take the proposed replacement fence; ACCEPT only if (a) every decorator line (@require/@ensure/@guard/@effects) is byte-identical to the original's (the contract and the cap never change, only the steps), and (b) the candidate run exits 0. Save an accepted fix to the host overlay (the store dir, never the authored SKILL.md and never the repository); a later plain `skill run` prefers the overlay and says so on stderr, as today's "running the host's learned version".
4. Seam: yoke must not import bashy. Add a yoke skills Option (e.g. WithBashSharpRunner(func(ctx, dir string, env []string, body string, stdout, stderr io.Writer) (exitCode int, err error))) that bashy sets in internal/agentos skillsOptions(); bashy implements it exactly like internal/agentos/dag_bashpp.go (syntax.LangBashPP parser, interp.Lang(LangBashPP), interp.Dir/Env, wireExec(...)). Without the option set, a Bash# unit fails with a clear "no Bash# runner in this host" error (not the old prose-only message).

RED/GREEN:
- yoke pkg/skills unit tests with a fake runner: a Bash# unit runs + attests; failing body -> non-zero + failed receipt; --adapt accepts a fix that keeps decorators and passes, REJECTS one that edits an @ensure/@guard line, saves to overlay; next run uses the overlay; skill.dhnt skills unchanged (existing tests stay green).
- bashy: a focused agentos test running a tiny fixture Bash# skill (an @ensure that passes and one that fails) through `skill run`.
- Live, by the worker from the umbrella root after installing nothing: build bashy to a scratch path and run `<scratch-bashy> skill run go-repo-health` in a tiny Go module fixture dir (NOT the whole umbrella: full go test is too heavy for Dragon) — red before, green after. The manager runs `skill run typeform-fill` on the live fixture.

RULES: Dragon = focused unit tests only (go test ./pkg/skills/..., go test -short ./internal/agentos -run <focused>); no podman, no full suites. Two repos in order: yoke first (push explicit SHA to main), then bashy (bump yoke pin in go.mod AND internal/agentos/src_mount.go srcModulePins together, run go build ./cmd/bashy with GOWORK=off). Commit trailers: Sprint: #512 / Story: #2820 / Story-ID: b8726cd1689c. Don't touch cmds/browser or pkg/browser. Report SHAs + test output in `sprint submit`.
