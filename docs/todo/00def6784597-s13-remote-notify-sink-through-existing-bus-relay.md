---
id: 00def6784597
kind: feature
title: S13 remote notify sink through existing bus relay
seq: 1901
status: done
priority: p2
labels:
    - yoke
    - bus
created: 2026-10-11T09:47:24.546685Z
weave: 100
assignee: claude-sonnet5.5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T10:00:43.743448Z
closed_by: codex-gpt6-sol
---

Scoped implementation child of umbrella S13 c06814664e20. Add generic notify sink kind mb:PERSON@HOST to yoke/pkg/bus NotifyEvent using the existing resolveRemote/sendRemote seam and its queued receipt semantics; do not add a cloudbox endpoint or mirror the whole log. Explicit remote addresses must resolve on the roster, refuse ambiguous/unknown/no relay without posting locally, preserve principal and subject-only size limit. Keep local notify target behavior unchanged. Focus pkg/bus/notify.go and pkg/bus/notify_test.go; no source overlap with current S418 Yoke token fix. Verify GOWORK=off go test -short ./pkg/bus/... including fake remote relay acceptance/refusal, exact subject/principal and queued-not-delivered evidence. Manager owns paired two-host live gate and original S13 closure. Commit trailers Sprint/Story/Story-ID/Agent, private managed run no push.
