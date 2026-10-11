---
id: 27235a3ad302
kind: feature
title: S11 generic typed HTTP tool-command transport for declarative Codync actions
seq: 1891
status: todo
priority: p1
labels:
    - yoke
created: 2026-10-11T08:21:16.691398Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Scoped child of S11 ee69f18dbb08 after rejected Bashy run #26. In yoke pkg/fleet command schema and pkg/toolcmd add a reusable HTTP command action (method, URL/path, JSON request body built with encoding/json, token read from a secret file/ref, request timeout, response handling) usable by any tool declaration. Enforce loopback target by default, explicit safe remote policy only via existing approval/effects machinery; no shell, eval, base64-encoded payloads or hand-built JSON. Keep existing exec/script modes and typed outcome/event contract. Test isolated fake loopback for bots/send/approve/usage including quotes/newlines, missing token, non-loopback refusal, HTTP failures and no secret leakage. Existing S9 capability routing must keep passing. Own pkg/fleet/commands.go and pkg/toolcmd; do not edit pkg/chat/acp, Bashy S16/S19, outpost S18. Verify GOWORK=off go test ./pkg/fleet/... ./pkg/toolcmd/... ./pkg/chat/... and run focused Mac/Linux package gate after source; worker commits with Sprint #418 and this Story-ID/Agent trailer. Manager will rework Codync YAML and run live Codync gate under original S11.
