---
id: 33f400bc9ac3
kind: feature
title: 'S7 Human notify sinks: exec sink on bus sidecar subscriptions; desktop/ntfy/webhook as Bash# skill functions'
seq: 1869
status: done
priority: p2
labels:
    - yoke
created: 2026-10-11T03:51:56.47153Z
weave: 81
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T07:42:33.088144Z
resolution: fixed
closed_by: codex-gpt5.6-sol-c
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Doorbell semantics, the inbox human lane, schedule.Job argv exec (notify --at/--in in yoke/pkg/bus/notify.go) and the trigger taxonomy (sprint wait events accepted/rejected/submitted/stage/blocker/idle; activity Status blocked/failed) exist; delivery to a human outside bashy does not. Add a sink option on bus sidecar subscriptions (yoke/pkg/bus/sidecar.go, today pending buffer or control socket only): --sink exec -- CMD and --sink tool-cmd TOOL:NAME (so openclaw:send or codync:send become sinks), running argv with the event JSON on stdin like schedule.Job. desktop (osascript/notify-send), ntfy and webhook are Bash# functions in skills/agent-hub.md behind exec. First slice allowed: a supervised bashy events --follow --kind approval,notice --json piped to the skill. Acceptance: exec sink receives the event JSON for an approval and a sprint blocker; tool-cmd sink calls a stub tool command; subject-only, no bodies leave the host.

Conductor verify: `go test ./pkg/bus/... ./pkg/room/...` in yoke and a process-level exec sink test with approval and blocker JSON; run tool-cmd stub on claimed native venue. Own bus sidecar/sink code and skills; S3 room stream already merged. Commit with `Agent:` trailer.

Manager acceptance 2026-10-11 07:44 UTC: Codex Sol continuation `b63487e` merged via guarded yoke weave #81. The manager reviewed argv resolution, subject-only external envelope, fallback queue on failure, process-level approval/blocker exec sink and tool-cmd stub tests, and desktop/ntfy/webhook skill functions. Independent claimed Mac `GOWORK=off go test ./pkg/bus/... ./pkg/room/...` PASS (`/tmp/s418-mac-s7-b63487e.log`). The earlier AGY environment-exit WIP remains preserved at `origin/agent/sprint418-s7-wip` but is not counted as delivery.

Continuation 2026-10-11 07:29 UTC: AGY run #77 exited -1 during active work and preserved WIP `7240454` on `origin/agent/sprint418-s7-wip`; no source was merged or accepted. Review and selectively cherry-pick that branch, complete the process-level exec and tool-cmd gates, and commit under this same Story-ID. The manager will run the native gate. Keep the first run's environment failure visible; do not count its WIP as verified delivery.
