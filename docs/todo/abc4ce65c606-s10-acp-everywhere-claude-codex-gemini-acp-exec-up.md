---
id: abc4ce65c606
kind: feature
title: 'S10 ACP everywhere: claude/codex/gemini acp_exec, updates as events, bashy acp with permission relay'
seq: 1872
status: assigned
priority: p2
labels:
    - yoke
    - bashy
created: 2026-10-11T03:52:03.478684Z
weave: 126
assignee: agy-gemini3.8-flash
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Add acp_exec to yoke/pkg/fleet/baseline/tools claude, codex and gemini via pinned ACP adapters (download records, sha256); map tool_call, plan, thought and usage updates into S3 entry events instead of dropping them (yoke/pkg/chat/acp.go); bashy acp = herald acp, relaying request_permission from S4; publish an ACP-registry manifest. Acceptance: Zed and a Codync bot whose command is bashy acp each complete a prompt; a permission request reaches bashy approve. Refs docs/herald-agent-interop-design.md.

Manager direct review 2026-10-11 14:35 UTC: Sonnet5.5 run #120 committed `f30d2b7` adding explicit `bashy acp --sandbox workspace-write` through the existing chat session/launcher path. The manager independently passed `GOWORK=off go test -short -count=1 -timeout 5m ./pkg/herald/... ./pkg/acp/... ./pkg/chat/...` and `git diff --check`, then pulled the run. The automatic `done` transition was reopened immediately: the required genuine Codync agent-originated S4 approval and Zed native prompt remain unverified. Keep the story open until those live gates pass; the prior read-only Codync attempt failed to create an approval record and is preserved in the sprint evidence.

Manager verdict 2026-10-11 12:35 UTC: Opus5 run #37 delivered top-level `bashy acp` at `9af12996`, merged by managed Weave at `9ec0ec62` after direct diff review, exact focused ACP JSON-RPC initialize/session/prompt process tests PASS, and required repository gate `go build ./... && go test ./internal/... ./tools/... && make test-bash` PASS (Bash 86/86). The first pull completed the gate but encountered only live generated assignment metadata in this story file; the manager backed it up to `/tmp/s418-s10-story-before-pull.md`, restored that metadata-only diff, and reran the gate. The old isolation warning remains in the pull record. Weave's automatic done transition marks this code merge; original S10 remains open for actual Zed and Codync prompts and a permission request reaching `bashy approve`. Earlier Yoke ACP adapters/selection event code remains integrated; do not claim live acceptance from this merge alone.

Manager continuation 2026-10-11 11:56 UTC: moved the same Story-ID from umbrella to Bashy for a pointed, private worker. Current Bashy `244740fe` runs `bashy herald acp` but has no top-level `bashy acp`; Yoke already wires `acp.ApprovalHandler` through `pkg/chat/acp.go`. Implement only the Bashy top-level dispatch/help parity and one process-level ACP initialize/session/prompt test in `internal/agentos/agentos.go`, `commands.go` and focused tests. Verify with `GOWORK=off go test -short -timeout 5m ./internal/agentos/...` plus a built `bashy acp --help` and fake-runner JSON-RPC exchange. Do not duplicate Yoke's existing permission relay or add a framework. Submit a bounded partial receipt; original S10 remains open for the pinned adapters/update events, real Zed and Codync bot prompts and an S4 permission request reaching `bashy approve`. Do not touch active Yoke S6 meet or S13 task-feed/ask files.

Worker receipt 2026-10-11 (Bashy, bounded partial — story STAYS OPEN). Delivered
only the Bashy half the continuation scoped: `bashy acp` is now a top-level
front door that prepends the subcommand and delegates to `herald.Run`, so it is
`bashy herald acp` with nothing of its own — no second server, no second
permission relay, no framework. Surface wiring: `directFrontDoorVerbs` (callable
as `bashy acp`, never a bare shell shim — a three-letter JSON-RPC server in every
interactive shell has no upside), `curatedHiddenVerbs` (it inherits herald's
experimental claim because it is herald's code), a `bashyOwnedVerbAtlas` row
(cross/orchestration/userland, caps json+needs-network+spawns-processes, effects
exec/net/spend — answering a client costs what dialling one costs), and a
`verbSynopsis` line. `docs/command-atlas.md` curated-hidden paragraph updated
22 → 23 with the no-shim exception stated.

Evidence (all run, not inferred): `GOWORK=off go test -short -timeout 5m
./internal/agentos/...` → ok (89.6s) for all three packages; `go test -run
TestACP ./internal/agentos` → ok (17.6s). `TestACPProcessLevelInitializeSessionPrompt`
drives the REAL built `bashy acp` process with yoke's `acp.Client` as the host:
initialize (strict v1 negotiation), session/new, session/prompt, a second
multiplexed session, and an unknown-session rejection.
`TestACPHelpIsHeraldACPHelpByteForByte` asserts `bashy acp --help` equals
`bashy herald acp --help` rather than copying the prose, so a future
`herald acp` flag is documented the day it lands. Manual transcript against the
built binary confirmed the same three responses on the wire; note that a
blindly piped transcript races (session/prompt can be served before session/new
returns), which is why the test uses a sequential client.

NOT delivered, and the reason this story stays open: the prompt turn is proven
only to the REFUSAL boundary — `chatRunner` refuses a whitespace prompt before
starting a local agent, so no agent binary, pty, or inference is involved. A
completed turn with real text is untested here. Still owed by the original S10:
`acp_exec` on the baseline claude/codex/gemini tools via pinned ACP adapters
with download records and sha256; mapping tool_call/plan/thought/usage updates
into S3 entry events in `yoke/pkg/chat/acp.go`; the published ACP-registry
manifest; and the live acceptance — real Zed and a real Codync bot each
completing a prompt, and an S4 `request_permission` reaching `bashy approve`.
Note `herald acp` deliberately unsets `BASHY_ACP` to prevent a nested ACP rung,
so the adapter work must not assume the server can drive an ACP agent to answer
its own request. Pre-existing environment failures observed and NOT caused by
this change: `TestE2EAllListedCommandsDispatch` fails on verb `service` because
`bashySelfPath()` resolves to a stale installed `~/.local/bin/bashy` (2026-10-10)
that predates the verb — reproduced at base commit `244f88e5`; and
`cmd/bashy TestYAMLAgentFenceCompilerRunner` fails on a missing `../ycode`
sibling workspace, also reproduced at base. `docs/herald-agent-interop-design.md`
does not exist in this repo.

Manager continuation 2026-10-11 13:06 UTC: the same original Story-ID moves
back from Bashy to Yoke because the remaining reverse ACP server and chat
bridge live in `pkg/herald`, `pkg/acp` and `pkg/chat`. Bashy's top-level front
door is already merged and pinned at `22d3a3ec`; do not recreate it. Baseline
`acp_exec` entries for Claude/Codex/Gemini and the ACP registry manifest already
exist. Direct live Mac gate against Bashy `131bd52a` and Yoke `db6c4f6`:
official Codync v2.13.2 queued real prompts to a disposable bot whose literal
command was `bashy acp`, but both ended `The agent failed: Internal error`.
ACP initialize and session/new succeeded. The default Claude turn was correctly
refused by the launch guard for uncontained `--dangerously-skip-permissions`;
a private safe plan-mode fleet override cleared that guard, then Claude exited
at its trust screen before PTY readiness. A separate read-only Codex PTY
probe printed PONG but did not finish a Codync ACP turn. No Zed binary was
available on the two checked Macs. The direct source audit found the S4
`ApprovalHandler` on the ACP client adapter path; an actual reverse-server
`request_permission` to `bashy approve` has not been proved.

Bounded continuation: inspect the existing reverse server and shared S4 queue;
make a real host permission request reach that queue and return the approved or
denied choice through the existing ACP protocol, with a process-level fake host
test for the round trip and denied/replayed choice. Keep the launch guard and
account checks intact. Resolve only the safe agent launch needed for a real
`bashy acp` prompt; use current fleet declarations and avoid a second
permission or chat framework. `go test ./pkg/herald/... ./pkg/acp/... ./pkg/chat/...`
is the source gate. Submit a bounded receipt; the manager owns the real
Codync/Zed/native `bashy approve` live gates and will keep S10 open until they
pass.

Manager partial verdict 2026-10-11 13:25 UTC: Opus5.5 run #118 source `e8feee6` was directly reviewed and merged as `87758aa`. It relays an approval raised by the reverse ACP session through `session/request_permission`, records the host answer on the same S4 ask record, and preserves first-decision-wins for a local `bashy approve`. The worker's process-level fake-host allow/deny/replay tests are in the source; manager independently ran `GOWORK=off go test -short -timeout 5m ./pkg/herald/... ./pkg/acp/... ./pkg/chat/...` on the exact private candidate: PASS. The manager amended the worker commit's missing `Agent:` trailer with its actual claimed identity and reverified the run; Weave's isolation warning names only generated assignment metadata in this story. Its mechanical `done` transition was restored to todo. The worker did not resolve the real Claude trust-screen exit or deliver a Codync/Zed final turn. A real host permission request and native `bashy approve` redemption still require live proof. Keep original S10 open; do not infer live completion from the fake-host test.

Manager 2026-10-11 14:29 UTC continuation: genuine Codync v2.13.2 bot and Codex turn at exact Bashy `974d9490`/Yoke `a0cf54c` reached `bashy confirm.bsh` and its destroy-effect guard, but `bashy acp --read-only` made Codex unable to create the S4 approval record (`operation not permitted`); no queue/card was raised. Without `--read-only`, the current default advertised YOLO mode and is not an acceptable acceptance path. Sanitized `/tmp/s418-s10-agent-origin-redacted.json`; private bot/token/host/children cleaned. Add only an explicit safe `--sandbox workspace-write` option to `pkg/herald/acp.go`, validate supported values and feed `chat.SessionOptions.Sandbox` through the existing `agentlaunch.ApplySandbox`. Preserve `--read-only` behavior, reject incompatible flags, and keep the no-bypass launch guard. Add a focused test that the real ACP command launches Codex with workspace-write and without bypass/YOLO argv; use current `pkg/herald` test fixtures rather than a new launcher layer. Verify `GOWORK=off go test -short -count=1 -timeout 5m ./pkg/herald/... ./pkg/acp/... ./pkg/chat/...`, `git diff --check`. Submit bounded source; manager owns fresh genuine Codync `@confirm` → S4 queue → permission card → allow/deny/replay gate and Zed native proof. Do not close original S10 for source alone. No edits in Bashy/Cloudbox/Outpost or Sprint420 files.

Conductor continuation 2026-10-11 20:03 UTC: user orders a bounded S10 qualified Mac Zed/Accessibility recovery. Source already passed genuine Codync allow/deny/once resume. Check actual claimed Mac GUI venue for installed Zed and usable Accessibility permission; perform a real permission roundtrip only with legitimate OS authority. Do not simulate GUI approval or weaken admission. Own S10 ACP/Zed evidence and a narrow fix only if a source defect is found; no edits to S9/S13 toolcmd/comms paths. Record exact venue, app/version, permission state, commands, raw outcome and blocker if operator elevation is needed. Commit/push any code in private Yoke with S10 trailers; manager reviews/integrates. Keep original S10 open if attended Zed is unproved.

Run `yoke#120` finished/merged its source slice (`f30d2b7`) and was unlinked before this Zed continuation. The real Codync allow/deny/once acceptance remains preserved. Original S10 was reopened because an attended Zed permission roundtrip is still unproved; do not redo the merged ACP source without a concrete defect.

Manager claude-opus5.5 2026-10-11 21:48 UTC: STILL OPEN, waiting on one attended operator prompt. Zed 1.23.2 is installed on Dragon (Homebrew cask) with `~/.config/zed/settings.json` agent server `bashy-s418` → `~/.bashy/s418-zed/bashy` (Bashy b51cf904, sha256 b0f0ea84…) `herald acp --agent codex-gpt6-luna --read-only --timeout 180s`; the ACP `initialize` smoke returned protocolVersion 1. Action: open the Zed Agent panel, pick `bashy-s418`, send "Reply with the single word OK"; then read `~/Library/Logs/Zed/Zed.log` and accept if the turn completed. Codync prompt and the approve relay already passed. AGY run #126 failed while looking for the then-absent Zed; not reassigned.
