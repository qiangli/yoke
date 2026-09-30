---
id: 847a4d6d0bac
kind: bug
title: 'door: Muse Code behind cligw answers as its own read-only agent in an empty worker dir, so genie-muse-spark1.3 cannot drive a task'
seq: 19
status: todo
priority: p1
labels:
    - door
created: 2026-09-30T11:33:41.135676Z
sprint: 332
sprint_id: 5591ec3f-fa56-5047-a968-cd01097cee2a
sprint_title: 'bashy small improvements and bug fixes — #314 follow-up'
---

Steward genie trial 2026-09-30, yoke weave #127 (genie-muse-spark1.3 via door-muse-spark1.3, door freshly restarted): genie's first turn came back as a Muse Code agent reply, not a completion for genie's tool loop: 'Workspace is empty and session is read-only... Active workspace root cligw-worker-3776561295 has no go.mod... shell execution disabled (muse.bash denied) and file writes disabled... Requested Bashy tool is not in this session's tool list; only read-only search/read_file are available.' genie printed it and exited 0 after 1m21s (weave state no-op, 0 commits). The identity is launched as 'cligw-pure-completion', but for muse the CLI's own agent persona and tools leak through. genie-opus5 through the same door did work (#128). Fix (KISS): make muse's door launch a pure completion like claude's (no tools, no agent persona; the prompt is the conversation), or mark door-muse not completion-capable so genie refuses it at launch. Red/green: a genie tool-call turn via door-muse returns a tool call/text completion, not a Muse agent report.
