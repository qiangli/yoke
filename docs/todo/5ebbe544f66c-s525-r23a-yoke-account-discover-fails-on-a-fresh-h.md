---
id: 5ebbe544f66c
kind: bug
title: 'S525.R23a yoke: account discover fails on a fresh host (cloudbox adapter errors when outpost absent and nothing cached) — blocks rc.23'
seq: 2940
status: done
priority: p0
labels:
    - release
created: 2026-10-30T04:17:16.206038Z
weave: 729
assignee: claude-opus5.5
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-30T04:22:48.001034Z
resolution: fixed
closed_by: claude-opus5.5-a
---

rc.23 release blocker found in release-preflight + bashy CI (test workflow red on main on ubuntu/macos/windows since the S524 account change): bashy internal/agentos TestAccountBuiltinAdaptersLinked runs 'account discover --dry-run --json' with a fresh BASHY_HOME and gets {error: adapter_failed, 'account discovery failed'} exit 1. Reproduced on Dragon with env -i + empty HOME. Root cause: yoke pkg/accounts/adapters/cloudbox Adapter.Discover — on a host with no outpost installed (a.run -> 'outpost not installed'), no profiles and nothing cached, it returns a.cached(lastErr), and cached() returns errors.New(code) when there is no previous observation; pkg/accounts/cmd.go then fails the whole discover on any adapter error. Discovery is local-only, so a host with no cloudbox/outpost configuration has simply no cloudbox account: return (nil, nil) in that case (no profiles, nothing cached, failure is 'not installed'/unreachable). Keep: cached accounts are still returned marked Status=Error with LastError when a previously known account can't be read. Do not change other adapters or the cmd-level policy. Tests: yoke unit test for the cloudbox adapter with an injected runner returning 'outpost not installed' + empty store -> no observations, no error; existing cached-error behaviour unchanged. Gate: go test ./pkg/accounts/... ; GOOS=windows go vet ./pkg/accounts/... . The manager bumps bashy's yoke pin + srcModulePins after merge and re-runs TestAccountBuiltinAdaptersLinked in a clean env.
