---
id: 36dc0a9782eb
kind: bug
title: 'S514.R4 Windows: a registered exec command with an MSYS-style absolute path reads as not installed'
seq: 2773
status: done
priority: p0
labels:
    - release
    - windows
created: 2026-10-27T11:48:59.003061Z
weave: 626
assignee: claude-sonnet5.5
sprint: 514
sprint_id: fadd1a46-0368-50f1-a65d-4ed147b17629
sprint_title: 'invitation email + scoped satellites (KISS/MVP: scope + expiry on the one-time pairing code; no outpost change)'
closed: 2026-10-27T12:02:46.915448Z
resolution: fixed
closed_by: claude-sonnet5.5-b
---

Found by the rc.17 Windows QA lane (bashsharp tour 11-fleet: agent-denied, model-read-denied and two more): every run prints 'warning: tool fleet-tour-tool: not installed: /c/Users/<user>/qa/<tag>/bin/bashy is not on PATH' although bin/bashy.exe is there and on PATH. Cause: pkg/fleet/commands.go Command.Executable(): on Windows filepath.IsAbs('/c/...') is false and exec.LookPath of the MSYS form fails (and the implicit .exe is not tried). Fix the presence check only (do not change how the command is executed, and do not add argv conversion; see the binmgr-only rule): on Windows resolve an MSYS-style drive path (/c/...) to its native form (reuse the existing pathconv helper the shell uses) and let LookPath apply PATHEXT. Unit test with a temp dir: Windows build tag or runtime.GOOS guard; exec record '/<drive>/.../tool' with tool.exe present -> present. Gate: go test ./pkg/fleet/... + GOOS=windows go vet ./pkg/fleet/; then on the Windows test host (address in ~/.bashy/s514-windows-test-host, outpost ssh key auth) build a bashy with this yoke and run the tour case: tour/11-fleet/agent-denied.bsh must print no 'not installed' warning. No hostnames in code/docs.
