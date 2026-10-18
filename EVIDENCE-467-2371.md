# EVIDENCE — Sprint #467, Story #2371 (5659e455cec0)

genie worker under weave reports health inconsistent (launch tool bashy vs recorded genie)

Agent: claude-fable5.1-w398 · weave run #398 · branch agent/weave-issue-398 · host: the development workstation (darwin)
Scope: health classification helpers + tests ONLY (`pkg/weave/weave_health.go`,
`pkg/weave/weave_health_frontdoor_test.go`). Launch prompts, runWeaveSay, socket card
principal (#396), clone-holder check (#395), worker attribution (#397) untouched.
No other session or queue record was modified; the live queue was only READ.

## Root cause

The fleet registry tool `ycode` (alias `genie`) launches through the bashy front door:
`cli.binary: bashy`, `exec: bashy genie -m {model} {prompt}`. weave records the registry
name (`tool: ycode` / `genie`) but the launch spec records the binary (`launch_spec.tool:
bashy`, `argv[0]: genie`). `weaveHealthConsistencyIssue` compared the binary name against
the recorded tool and reported every live ycode/genie run as
`owner launch tool "bashy" contradicts recorded tool "ycode"` — while the wrapper was
alive and smoke passed.

Live record (read-only, `~/.bashy/weave/yoke-d9ca9236/queue.json`, run #395):
`tool=ycode owner=ycode-glm-5.3-e launch_spec.tool=bashy argv=[genie -m glm-5.3 …]`.

## Fix

`weaveLaunchSpecRunsTool(recorded, spec)`: direct binary comparison as before; when the
launch binary is the `bashy` front door, the tool is the verb in `argv[0]`, compared by
name and then reconciled through the fleet registry (`Catalog.Tool` resolves alias
`genie` → canonical `ycode`). A front door with no verb, a flag in place of a verb, or
a verb resolving to a different registry tool is still a contradiction; unknown names
never match. The registry is read only when the direct comparison fails.

## RED (reproducer first)

```
$ BASHY_HINTS=off go test ./pkg/weave -run 'TestHealthAcceptsBashyFrontDoor|TestHealthLiveFrontDoor|TestHealthStillFlagsForeignToolBehindBashyFrontDoor' -count=1
--- FAIL: TestHealthAcceptsBashyFrontDoorForRecordedYcodeOrGenie (0.00s)
    weave_health_frontdoor_test.go:54: recorded "ycode" launched "bashy" genie: healthy run reported "owner launch tool \"bashy\" contradicts recorded tool \"ycode\""
    weave_health_frontdoor_test.go:54: recorded "genie" launched "bashy" genie: healthy run reported "owner launch tool \"bashy\" contradicts recorded tool \"genie\""
    weave_health_frontdoor_test.go:54: recorded "ycode" launched "/Users/u/.local/bin/bashy" genie: healthy run reported "owner launch tool \"/Users/u/.local/bin/bashy\" contradicts recorded tool \"ycode\""
    weave_health_frontdoor_test.go:54: recorded "genie" launched "/Users/u/.local/bin/bashy" genie: healthy run reported "owner launch tool \"/Users/u/.local/bin/bashy\" contradicts recorded tool \"genie\""
    weave_health_frontdoor_test.go:54: recorded "ycode" launched "C:\\Users\\u\\.local\\bin\\bashy.exe" genie: healthy run reported "owner launch tool \"C:\\\\Users\\\\u\\\\.local\\\\bin\\\\bashy.exe\" contradicts recorded tool \"ycode\""
--- FAIL: TestHealthAcceptsBashyFrontDoorYcodeVerb (0.00s)
    weave_health_frontdoor_test.go:65: bashy ycode launch recorded as ycode reported "owner launch tool \"bashy\" contradicts recorded tool \"ycode\""
--- FAIL: TestHealthLiveFrontDoorRunIsNotInconsistent (0.00s)
    weave_health_frontdoor_test.go:83: live front-door run classified inconsistent: {Health:inconsistent Reason:owner launch tool "bashy" contradicts recorded tool "ycode" Next:repair the launch record before resuming …}
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	0.765s
```

(`TestHealthStillFlagsForeignToolBehindBashyFrontDoor` already passed before the fix — the
foreign-mismatch guard is pinned, not newly introduced.)

## GREEN

```
$ BASHY_HINTS=off go test ./pkg/weave -run 'TestHealth|TestWeaveHealth' -count=1 -v | grep -E '^--- '
--- PASS: TestHealthAcceptsBashyFrontDoorForRecordedYcodeOrGenie (0.08s)
--- PASS: TestHealthAcceptsBashyFrontDoorYcodeVerb (0.00s)
--- PASS: TestHealthLiveFrontDoorRunIsNotInconsistent (0.04s)
--- PASS: TestHealthStillFlagsForeignToolBehindBashyFrontDoor (0.13s)
--- PASS: TestHealthAcceptsManagedBinaryPathForRecordedTool (0.00s)
--- PASS: TestHealthStillFlagsADifferentTool (0.00s)
--- PASS: TestWeaveHealthDeadProcessIsStale … TestWeaveHealthTerminalStateWithoutEvidenceIsInconsistent (all PASS)
ok  	github.com/qiangli/yoke/pkg/weave	2.234s

$ BASHY_HINTS=off go vet ./pkg/weave
(exit 0)
```

Tests are isolated: the fleet registry is pinned with `fleettest.Ring` + `fleet.WithRoot`
(embedded baseline under a temp root; the developer's real store is never read).

## Live-record smoke (read-only, temporary test deleted before commit)

```
#389 state=working recorded="claude" launch.tool="$HOME/.bashy/tools/claude/2.1.283/claude" argv0="--dangerously-skip-permissions" old=true new=true
#395 state=killed  recorded="ycode"  launch.tool="bashy" argv0="genie" old=false new=true
#398 state=working recorded="claude" launch.tool="$HOME/.bashy/tools/claude/2.1.283/claude" argv0="--dangerously-skip-permissions" old=true new=true
```

`old` = previous `weaveSameLaunchTool(recorded, launch.tool)`; `new` = `weaveLaunchSpecRunsTool`.
The real ycode/genie record is now accepted; claude records are unchanged. (#389 was
re-bound to claude after the original genie incident; its current record reflects that.)

## Not run here (manager's remote gate)

Whole-package `go test ./pkg/weave`, `scripts/crossvet.sh`, `scripts/ci-test-gate.sh` —
per the story instruction: short filtered tests only on the development host, no heavy build.
