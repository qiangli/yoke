---
id: 34620f993c81
kind: bug
title: 'door: a tool-calling request to door-muse-* surfaces 503 breaker text instead of the seat''s 400 refusal, and genie does not refuse door-muse at launch'
seq: 24
status: wontfix
priority: p2
labels:
    - door
created: 2026-09-30T16:05:01.586041Z
closed: 2026-09-30T16:27:09.726696Z
---

Sprint 340 conductor 2026-09-30: after yoke 5c0930f (847a4d6d fix b), POST /v1/chat/completions with tools to model muse-spark1.3 -> 400 "seat ... is not completion-capable for tool-calling requests" (good), but to model door-muse-spark1.3 -> 503 "no matching candidate is installed and outside breaker cooldown". Fix: the gateway passes a non-retryable 400 through the alias unchanged (and does not trip the breaker on it); genie checks cligw.ToolCallingCapable(broker AgentInfo.Tool) and refuses a muse door at launch. Red/green: door alias returns the 400; genie -m door-muse-spark1.3 exits non-zero with the message before any model call.

Closed 2026-09-30 by the Sprint 340 conductor: obsolete - yoke 30366b6 (story 3c5bbbb6) removed the muse tool-calling refusal, so there is no 400 left to pass through; genie -m door-muse-spark1.3 pong now answers in 29 s.
