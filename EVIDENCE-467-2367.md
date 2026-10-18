# Evidence — Sprint #467 Story #2367 (e39b9c3321f2): weave fleet --probe false UNUSABLE

Host: development macOS host (darwin). Worker: claude-opus5.5-w392. Date: 2026-10-18.
Tools: codex-cli 0.157.1 (/opt/homebrew/bin/codex), agy (~/.local/bin/agy),
opencode 1.18.30 (/opt/homebrew/bin/opencode, Homebrew build).

## Diagnosis

The smoke parser (`strings.Contains(stdout, fleet.SmokeToken)` in
`pkg/weave/weave_fleet.go` probeToolCapability) is correct. The false negative
comes from **binding selection**. `fleet.Catalog.SmokeArgv` took the FIRST
name-sorted agent for the tool and did not check retirement:

| tool | binding smoked (before) | why it fails |
|---|---|---|
| codex | `codex-gpt-5.5` → `--model gpt-5.5` (model retired; agent Unavailable) | 400: `The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account.` |
| agy | `agy-gemini3.1` (agent retired) → `--model gemini-3.1-pro-high` | `error: invalid model selection ... gemini-3.1-pro-high is not recognized` |
| opencode | `opencode-deepseek-v4-pro` (first live; metered) | not run: metered and not a subscription tool (brief) |

## Captured smoke output (exact probe argv, run directly in /tmp/s2367)

- `codex exec --skip-git-repo-check --sandbox workspace-write --model gpt-5.5 "Reply with exactly: SMOKE-OK"`
  → exit 1, empty stdout, stderr `ERROR: {"type":"error","status":400,...not supported when using Codex with a ChatGPT account."}`
- `agy --dangerously-skip-permissions --print-timeout 40m --model gemini-3.1-pro-high -p "..."`
  → exit 1, empty stdout, `error: invalid model selection`
- `codex exec ... --model gpt-5.6-sol "..."` → exit 0, stdout `SMOKE-OK` (9s)
- `agy ... --model gemini-3.8-flash-high -p "..."` → exit 0, stdout `SMOKE-OK` (9s)
- `opencode run --auto --model zai-coding-plan/glm-5.3-flash "..."` → exit 1, empty stdout,
  stderr `Error: {"name":"UnknownError","data":{"message":"Unexpected server error..."}}`.
  This is a genuine failure (the known-broken opencode-glm on Homebrew 1.18.30), so
  rejecting it as UNUSABLE is correct. The fix does not change that.

## Fix (minimal)

`pkg/fleet/verify.go` SmokeArgv now skips agents that are retired
(`CheckRetired(KindAgent)`) or whose tool or model dependency is retired
(`Agent.Unavailable`). The smoke check itself is unchanged: stdout must contain
`SMOKE-OK`, and a zero exit is not success.

## Red → green

`go test ./pkg/fleet -run 'TestSmokeArgvSkipsRetiredBindings' -count=1` (before the fix):
```
--- FAIL: TestSmokeArgvSkipsRetiredBindings (0.00s)
    verify_smoke_test.go:33: argv = ["smoke" "--model" "gone-target" "Reply with exactly: SMOKE-OK"], want ["smoke" "--model" "live-target" "Reply with exactly: SMOKE-OK"]
FAIL
FAIL	github.com/qiangli/yoke/pkg/fleet	0.707s
FAIL
```
Same command after the fix (-v):
```
=== RUN   TestSmokeArgvSkipsRetiredBindings
--- PASS: TestSmokeArgvSkipsRetiredBindings (0.00s)
PASS
ok  	github.com/qiangli/yoke/pkg/fleet	0.689s
```
Existing weave smoke regression (failures are still rejected):
`go test ./pkg/weave -run 'Smoke|FleetReports|FleetDefault|FleetProbe' -count=1`
```
ok  	github.com/qiangli/yoke/pkg/weave	1.492s
```
`go test ./pkg/fleet -run 'Smoke|Verify|Retire' -count=1` ; `go vet ./pkg/fleet` (clean)
```
ok  	github.com/qiangli/yoke/pkg/fleet	1.080s
```

## Live positive proof through the real probeToolCapability (throwaway test, not committed)

pkg/weave's TestMain swaps HOME for a temp dir, and under that the CLIs lose their
subscription auth (codex 401, agy OAuth prompt). The throwaway test therefore
restored the real HOME (`REAL_HOME=$HOME go test ./pkg/weave -run TestZZLiveProbe -v`).
The "before" run had the fix stashed:
```
--- BEFORE fix
codex model=gpt-5.5 capable=false dur=3s
agy model=gemini-3.1-pro-high capable=false dur=4s
--- PASS: TestZZLiveProbe (7.19s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	7.822s
--- AFTER fix
codex model=gpt-5.6-sol capable=true dur=7s
agy model=gemini-3.8-flash-high capable=true dur=6s
--- PASS: TestZZLiveProbe (13.18s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	13.970s
```
