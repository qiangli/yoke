---
id: 6fe9a6d49878
kind: bug
title: weave workers commit with identity 'unknown' when launched without BASHY_AGENT -> commit-attribution bypass events on the sprint card
seq: 1970
status: todo
priority: p1
created: 2026-10-12T08:01:02.398399Z
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

S423: every weave worker commit (yoke runs 152/154/155/156, cloudbox run 3) logged '(bypass) commit: Story #N assignee <worker>, found identity unknown'. Managers must strip BASHY_AGENT on weave start (else workers inherit the manager seat, S329/S422), but weave then sets no identity for the child. weave start should export BASHY_AGENT=<worker instance> to the agent process.
