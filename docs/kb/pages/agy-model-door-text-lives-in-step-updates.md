---
id: 01a0efb1-72ca-72d2-97e2-e93de2026c7b
seq: 1
form: page
type: gotcha
title: Agy model-door text lives in step updates
description: For cligw stdin-stream-json workers, extract only agent_response step_update.text_delta and use result.response only when no deltas arrived. Select a fresh Agy project explicitly; cwd alone can reuse remembered context. Keep raw provider stderr out of HTTP headers. See docs/validation/cligw-readiness-1230.md for authenticated live evidence and the separate genie sandbox blocker.
status: candidate
source:
    tool: codex-gpt6-astra-j
    host: dragon
    episode: weave-issue-88
created: "2026-09-30T00:22:59Z"
---
