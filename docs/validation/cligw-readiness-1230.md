# Authenticated model-door readiness — Story #1230

Sprint #328, Story-ID `84389f905a84`. Measured 2026-09-29 on dragon.

## Baseline and diagnosis

The reported installed baseline `7e27509`, after `bashy llm up` and the
owner credential named `bashy-llm`, was:

| Binding | Reported result at 45 seconds |
| --- | --- |
| genie-gpt-5.5 | pass |
| genie-opus5 | timeout |
| genie-gemini3.8-flash | provider response has no content |
| genie-muse-spark1.3 | timeout |

The installed binary during this investigation reported `b328cf1`.
The existing host gateway was left running. Tests use a separate authenticated
gateway built from this checkout, temporary state, and isolated listener ports.

The fleet YAML resolves the doors through `genie` to these CLI bindings:

| Door model | Worker binding | Provider model | Transport / terminal |
| --- | --- | --- | --- |
| door-claude-opus5 | claude-opus5 | claude-opus-5 | stdin stream JSON / type=result, is_error=false |
| door-agy-gemini3.8-flash | agy-gemini3.8-flash | gemini-3.8-flash-high | stdin stream JSON / event=result, result.status=SUCCESS |
| door-muse-spark1.3 | muse-spark1.3 | muse-spark-1.3 | cold exec JSON / payload_type=run.terminal.completed, payload.terminal=completed |

All three API door records use `api_key_ref: bashy-llm` and the corresponding
`http://127.0.0.1:24556/sticky/<genie-agent>/v1` URL. Those identities and
credentials are unchanged.

Agy's measured answer events have `step_update.step_type=agent_response` and
`step_update.text_delta`, including the DONE update. The terminal repeats the
answer in `result.response`. Neither text path was previously decoded, so a
successful CLI turn became an empty provider response. `cwd` alone also allowed
Agy to reuse a remembered project. The worker now explicitly selects a fresh
project rooted in its temporary directory.

Claude already decoded partial events and suppressed the following assistant
snapshot. It now also accepts terminal-only `result` text. Muse already decoded
MSP deltas and terminal text; its door invocation now excludes foreign personal
context, session logs, shell, writes, and web tools. The original Claude/Muse
45-second timeouts were not reproduced in the isolated live gateway; no claim
is made that a particular parser bug caused those historical timeouts.

Provider stderr no longer enters worker errors or HTTP error headers. User
message echoes, thinking, and tool payloads are excluded from answer extraction.
Fixtures contain synthetic content only. Live probes retain statuses/timings,
not prompts, replies, credentials, or raw streams.

## Validation

- `GOCACHE=/tmp/issue88-go-cache go test ./pkg/cligw/... ./pkg/fleet/...`: PASS.
- `GOCACHE=/tmp/issue88-go-cache go vet ./pkg/cligw/... ./pkg/fleet/...`: PASS.
- `CLIGW_LIVE_READINESS=1 GOCACHE=/tmp/issue88-go-cache go test ./pkg/cligw -run '^TestLiveDoorReadiness$' -v -count=1`: PASS.

The opt-in live test sends an authenticated HTTP request to the actual gateway,
launches the real subscription CLI, and requires an exact fixed readiness
answer, not merely exit zero or HTTP 200:

| Worker | Authenticated readiness | Duration |
| --- | --- | --- |
| claude-opus5 | PASS | 4.337 s |
| agy-gemini3.8-flash | PASS | 6.784 s |
| muse-spark1.3 | PASS | 20.632 s |

Regression coverage includes streamed and terminal-only responses for all three
providers, no duplicate terminal text, failed/cancelled terminals even at exit
zero, Agy cached-token accounting, prompt/thinking exclusion, stderr secrecy,
and launch isolation. The loopback test now uses an ephemeral port internally;
the public listener's default port behavior is unchanged.

## Remaining environment-only failure

`bashy agent verify --live --json` was run for all four genie bindings, including
the previously passing GPT control. All fail before a provider request in this
worker sandbox. Initial failures were host room and budget lock writes. Moving
those stores to temporary paths and turning off output spilling exposed an
unwritable inherited Go cache; setting the writable GOCACHE above resolved it.
The remaining exact failure, reproduced by invoking genie directly, is:

```text
genie: mkdir /Users/qiangli/.bashy/genie/chat/issue-88__Users_qiangli_.bashy_weave_yoke-d9ca9236_workspaces_issue-88-ecf051eb: operation not permitted
```

Genie's workspace-config creation uses the real home directory even when
`BASHY_HOME` is temporary. This is outside this checkout's writable roots.
No host permissions or credentials were changed. Full genie live verification
therefore remains unproven; rerun the four bindings after integrating/rebuilding
and restarting the door in the owning host session. Do not mark the fleet's
historical failed-verification notes green based solely on the gateway test.
