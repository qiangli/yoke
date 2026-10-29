---
id: ff213876daa1
kind: test
title: 'S525.A5r yoke: revoked-grant + preview-escape adversarial cases (A5 acceptance)'
seq: 2910
status: done
priority: p1
created: 2026-10-29T09:44:21.979976Z
weave: 691
assignee: codex-gpt5.6-sol
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T09:53:39.868872Z
resolution: fixed
closed_by: codex-gpt5.6-sol-o
---

Completes S525.A5 acceptance (umbrella 5c7b49ea6ae9) on yoke main a810260f. Extend pkg/webconsole/shaped_production_test.go TestShapedProductionGates subtests; keep all other subtests.
(1) 'revoked grant': BEFORE revoking, create a candidate for alice; after pairs.revokeAll() assert (a) POST /api/shape/notes/candidates refused 401/403, (b) POST .../candidates/<existing>/approve refused 401/403, (c) the shaped app api call (GET/POST /notes/api/list) refused 401/403, (d) alice ActiveRevision unchanged.
(2) 'preview escape': a candidate whose app/api/add.md script tries to (i) write alice's REAL data dir (absolute path from aliceStore.DataDir()), (ii) write bob's data dir, (iii) write $BASHY_HOME/escape.txt, (iv) open a TCP connection to a non-loopback TEST-NET address (192.0.2.1:80) — run via the preview route; each must be denied by containment (file absent afterwards; connect denied), alice+bob real data unchanged.
If process-level containment cannot deny one of these, do NOT weaken the assertion: make the shaped runtime REFUSE preview/activation when the containment level cannot enforce it (unsupported containment blocks, never masquerades), assert that refusal, and say so in the commit message. Production code fixes for a found hole are in scope (pkg/shape/contain, pkg/webconsole/shaped*.go only).
Acceptance: go test ./pkg/webconsole -run TestShapedProductionGates -count=1 and go test ./pkg/shape/... green; do not run the whole webconsole suite. Trailers Sprint: #525 / Story: #<seq> / Story-ID.
