---
id: 4da3ffa2e94a
kind: task
title: 'S531.D4z llm share: cloudbox app presence check ignores host (stale 127.0.0.1:17788 + port 24556 treated as present -> 502)'
seq: 2927
status: done
priority: p0
created: 2026-10-29T17:57:58.409161Z
assignee: claude-sonnet5.5
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T20:16:03.400032Z
closed_by: s531-manager
---

Found in D4 live 10-29. (1) pkg/broker/share.go ensureCloudboxApp matches only name+port+enabled+!login. On Dragon, outpost apps list --json had llm host '127.0.0.1:17788' port 24556 (target 127.0.0.1:17788:24556) from the old hand wiring, so cloudbox returned 502 and share kept it as present. Fix: also require host 127.0.0.1/localhost without an embedded port; otherwise re-add with --url http://127.0.0.1:PORT. (2) outpost 26430aa (S531.D4o) now prints 'SERVICE <- peer PEERID -> ADDR [live|down]' in 'outpost mesh consume ls'. ensureMeshForward's 'len(f) == 6' parser must accept the optional trailing state field (len 6 or 7), reuse only a [live] (or stateless) consume, and not add a duplicate. (3) When the peer's PATH bashy lacks 'llm door import' (version skew), report 'the bashy on PEER is older than this door; upgrade it (or --print | <candidate> llm door import)' instead of the raw cobra error. Red/green unit tests via the outpostRun seam in share_test.go; go test ./pkg/broker only. Reassigned from run #712 (no progress in 15 min).
