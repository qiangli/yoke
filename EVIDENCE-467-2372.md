# Evidence — Sprint 467 Story #2372 (e368300f12d2)

Preserve weave run logs across relaunch attempts.

## Problem

Relaunching the same weave run truncated `logs/issue-N.log`, destroying the
previous attempt's output (e.g. run 389's failed attempt evidence).

Root cause: `weaveOpenCaptureLog` (`pkg/weave/capture_redaction.go`) opened
the capture log with `O_TRUNC`. Additionally, `weave status`
(`pkg/weave/weave_impl.go`) never exposed the capture log path, so status
could not point at launch evidence.

## Fix (minimal, append)

- `pkg/weave/capture_redaction.go`: `weaveOpenCaptureLog` opens with
  `O_APPEND` instead of `O_TRUNC`. Each launch already writes its own
  `[weave] launch started` marker, so one file now holds every attempt.
- `pkg/weave/weave_impl.go` (`runWeaveStatus`): expose `log_path` in JSON
  and a `log:` line in text output, falling back to the conventional
  `logs/issue-N.log` when present. Additive only; existing contracts
  unchanged.

## Red (before fix)

`go test ./pkg/weave/ -run TestWeaveCaptureLogRelaunchRetainsBothAttempts -v -count=1`

```
--- FAIL: TestWeaveCaptureLogRelaunchRetainsBothAttempts (0.00s)
    capture_redaction_test.go:225: relaunch lost "first-attempt-output";
    log holds "[weave] launch started agent=second\nsecond-attempt-output\n"
```

## Green (after fix)

`go test ./pkg/weave/ -run
'TestWeaveCaptureLogRelaunchRetainsBothAttempts|TestWeaveStatus_ShowsLogPath|TestWeaveStatus_ShowsResultPath|TestWeaveCaptureLogIsOwnerOnlyWhenSecretsRendered'
-v -count=1` — all PASS.

`go test ./pkg/weave/ -run
'TestWeaveCapture|TestWeaveStatus|TestWeaveLog|TestWeaveRunEnd|TestWeaveLaunch'
-count=1` — ok (29s). `go vet ./pkg/weave/` clean. `gofmt -l` clean.

## Scope notes

- Touched only log-creation and status hunks; no prompt/say/socket,
  handoff, health, or identity code.
- No other live run altered; no session restarted.
- Full touched-package gate left to the manager (remote).
