---
id: 1f7234a6bb83
kind: feature
title: S531.D1 `delegate` tool in genie over registered vendor CLI commands
seq: 2888
status: done
priority: p0
labels:
    - genie
created: 2026-10-29T07:01:15.230273Z
weave: 676
assignee: codex-gpt6-astra
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T07:59:20.330714Z
resolution: fixed
closed_by: codex-gpt6-astra-z
---

Spec: dhnt umbrella plan §D0 (decided f7d071321). Depends: D0 (accepted), D2 (merged yoke c0a83cfd).

Slice 1 DONE: yoke c2f48343 (merged cf06659d) — guards in pkg/chat/delegate_guard.go: allow policy, subscription-login check (claude/codex), metered env scrub, idle timeout, structured error.kind envelope with receipt/identity/exit; required budget/token caps fail closed.

Slice 2 (this run, yoke pkg/chat only):
- Build the identity via capability.DelegateIdentity(tool, model) instead of the local string concat in delegateGuard.check.
- When caller is genie (BASHY_DELEGATE_CALLER=genie) and the vendor actually launched, record EXACTLY ONE ladder event via capability.RecordDelegateRun(tool, model, ev) after the run (outcome: success when exit 0, else failure; duty code; points 1). No event for denied/auth/pre-launch failures. Never write under the vendor's own tool:model or ycode:*. Allow a test seam for the store path (e.g. BASHY_LADDER_STORE or an injectable func) so tests never touch $HOME/.bashy/ladder.
- Non-genie callers: no ladder event from delegate (unchanged behavior).
Acceptance: unit tests with the fake vendor binary: genie caller success -> one event under genie-delegate:<tool>-<model>; failure -> one failure event; denied -> zero events; non-genie -> zero events. `go test ./pkg/chat/` green (run with BASHY_SPRINT_LEASE_TOKEN/BASHY_MINT_SPRINT unset; TestUnmanagedChildEnvStampsNoSeat is env-sensitive). Generic commit wording; no hostnames.
