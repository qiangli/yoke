---
id: 01a11c9a-1e85-738e-9812-25845bbc031a
seq: 2
form: page
type: lesson
title: CLI stdout ownership must outlive process wait
description: WHEN streaming events from a short-lived CLI with os/exec, do not let exec.Cmd.Wait own and close StdoutPipe while a separate scanner drains terminal events. Attach an explicitly owned os.Pipe writer to Cmd.Stdout, close the parent's writer after Start, and let the scanner close the reader after EOF; otherwise fast exits intermittently lose the terminal event and appear as unrelated routing/backend failures.
status: candidate
source:
    tool: codex-gpt5.6-sol-i
    host: dragon
    episode: weave-issue-35
created: "2026-10-08T17:40:24Z"
---
