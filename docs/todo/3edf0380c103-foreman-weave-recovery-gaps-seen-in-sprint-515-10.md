---
id: 3edf0380c103
kind: bug
title: 'foreman/weave recovery gaps seen in Sprint 515 (10-29/30): managed manager stalls, supervisor relaunch kills workers, dead runs unrecoverable'
seq: 2942
status: todo
priority: p1
labels:
    - weave
    - foreman
created: 2026-10-30T12:41:32.692203Z
---

Observed in Sprint 515 (operator session supervising s515r-manager), 2026-10-29/30. One bug cluster, fix together:

1. Managed-session control socket dies or times out while the agent is still alive: `foreman tell` -> `read unix ...ctl.sock: i/o timeout` or `connect: no such file`; `foreman status` reports idle/done while the claude TUI is mid-turn. Happened 3+ times; each stall cost 10-30 min until a human noticed. mb mail still reached the session.
2. Supervisor "stalled manager" recovery stops the managed session and its relaunch fails ("did not open its control socket" / "stopped by operator"), and the stop KILLED all linked weave workers (cloudbox#195/#196, dhnt#200) mid-flight, including one mid outpost-upgrade on a test satellite.
3. Those legacy runs are then unrecoverable: `weave start --resume`, `weave kill`, `weave reconcile` all refuse ("child birth identity and isolated group were not recorded at launch"). Only `weave abandon --disposition superseded` releases them.
4. `sprint assign --override` bypasses lease authorization but NOT the story->run link ("story is closed or already linked to run #N"); `sprint reassign` refuses too, so managers cannot relaunch a story without abandoning.
5. Supervisor misclassified a 48m max-runtime kill as "vendor-outage" and attempted a takeover (failed: "requires a terminal failed run, got done").

Acceptance: a stalled control socket is detected and the session restarted WITHOUT touching linked workers; resume/kill/reconcile work for runs launched before birth-identity recording (or a documented operator release verb); assign --override (or a reassign verb) can relink a story whose run is terminal; max-runtime kills are not classified as vendor outages. Red/green tests per item.
