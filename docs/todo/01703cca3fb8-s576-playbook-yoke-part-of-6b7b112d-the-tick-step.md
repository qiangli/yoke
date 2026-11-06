---
id: 01703cca3fb8
kind: doc
title: 'S576 playbook (yoke part of 6b7b112d): THE TICK step 5 delegates gates'
seq: 3049
status: done
priority: p0
labels:
    - sprint
created: 2026-11-06T17:46:19.98836Z
weave: 2
assignee: claude-sonnet5.5
sprint: 576
sprint_id: 0dcfdbeb-04ba-549f-96e0-a3febc9a6618
sprint_title: 'Proactive sprint manager: wake, supervise, review'
closed: 2026-11-06T18:48:34.610417Z
closed_by: s576-manager
---

Part (1) of S576 story 6b7b112d, which lives in yoke: pkg/weave/weave_story.go ~536 THE TICK step 5 says 'run the gate YOURSELF'. Change to: 'workers run gates; verify the verdict from bashy evidence; you run only the frozen-candidate final gate'. Update any golden/test text pinning the old wording. Unit tests: GOWORK=off go test ./pkg/weave on the self-hosted test host (never the dev box); record the result as verify evidence. Commit in yoke only with Sprint/Story trailers; do NOT bump bashy's yoke pin (s576-manager sequences that after S563's yoke bump).
