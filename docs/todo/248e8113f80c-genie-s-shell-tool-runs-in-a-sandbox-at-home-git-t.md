---
id: 248e8113f80c
kind: bug
title: genie's shell tool runs in a sandbox at <home>/git/terminal-bench, not the invoking workspace, so every genie edit is lost
seq: 23
status: todo
priority: p0
labels:
    - genie
created: 2026-09-30T15:09:45.121223Z
sprint: 340
sprint_id: 07abaf0d-4c54-57e7-b324-8c926b4c300f
sprint_title: Pure-Go m4, localedef and lp; listing view for the optional external POSIX tools
---

Steward 2026-09-30, reproduced twice from a scratch git repo with the installed bashy (genie bundle builtin 44d892ff6968): 'bashy genie -m door-codex-gpt-5.5 "run: pwd; echo x > probe2.txt; ls -la probe2.txt; echo HOME=$HOME"' -> tool call {script:...} ran and printed pwd = <home>/git/terminal-bench and the file listed there, but probe2.txt never appears in the invoking directory, and <home>/git/terminal-bench does not exist on the host - the tool executes in some sandbox rooted at a stale terminal-bench path. Every startup also logs 'dag: effect cap: target "chat": "dag -f" / "bashy.real" needs unknown not in Effects'. Consequences: all genie work today was void - steward trials yoke #126-128 (no file written in the workspace), sprint genie shadows on #110/#319, and #319 run #15 (genie glm-5.3, 12 min, zero workspace changes). Fix: the genie chat/solve tool's cwd and writable root = the invoking workspace (agent.yaml workspace '.' resolved against the caller's cwd); find where the terminal-bench root is injected (sandbox mount / stale session or bundle config). Red/green: 'bashy genie ... echo x > f' in a temp repo creates ./f.

Conductor root cause 2026-09-30 (Sprint 340, claude-opus5.5): NOT a sandbox or cwd problem. Harness session events for the steward probe (run 2df98cd5) show llm.requested -> llm.completed -> turn-committed with NO bashy.run event: genie never executed a tool. The door's completion was hasToolCalls:false with text '{"script":"pwd; ..."}' followed by an invented result ('<home>/git/terminal-bench', a fake ls line) - the model hallucinated the tool output. Re-probe on the current install (bashy 2db80ed): text '{"script":"pwd; echo x > probe2.txt; ls -la probe2.txt"}Done.', no file. The model emits the tool ARGUMENTS as bare JSON (plus trailing prose) instead of the {"tool_calls":[{"name":..,"arguments":..}]} envelope, and yoke pkg/cligw backend.go toolEnvelopeStart only recognises the wrapped form, so the call degrades to plain text. Fix lies in pkg/cligw (envelope recovery and/or the tool instruction), red/green on a recorded codex answer.
