---
id: 33f400bc9ac3
kind: feature
title: 'S7 Human notify sinks: exec sink on bus sidecar subscriptions; desktop/ntfy/webhook as Bash# skill functions'
seq: 1869
status: todo
priority: p2
labels:
    - yoke
created: 2026-10-11T03:51:56.47153Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Doorbell semantics, the inbox human lane, schedule.Job argv exec (notify --at/--in in yoke/pkg/bus/notify.go) and the trigger taxonomy (sprint wait events accepted/rejected/submitted/stage/blocker/idle; activity Status blocked/failed) exist; delivery to a human outside bashy does not. Add a sink option on bus sidecar subscriptions (yoke/pkg/bus/sidecar.go, today pending buffer or control socket only): --sink exec -- CMD and --sink tool-cmd TOOL:NAME (so openclaw:send or codync:send become sinks), running argv with the event JSON on stdin like schedule.Job. desktop (osascript/notify-send), ntfy and webhook are Bash# functions in skills/agent-hub.md behind exec. First slice allowed: a supervised bashy events --follow --kind approval,notice --json piped to the skill. Acceptance: exec sink receives the event JSON for an approval and a sprint blocker; tool-cmd sink calls a stub tool command; subject-only, no bodies leave the host.
