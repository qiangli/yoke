---
id: a76299332d33
kind: bug
title: ollama serve inherits the door client OLLAMA_HOST and binds the model door port 24556, so the door never starts
seq: 31
status: done
priority: p1
labels:
    - genie
    - llm
created: 2026-10-08T21:00:39.207296Z
assignee: claude-opus5.5
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-09T08:52:53.534958Z
closed_by: claude-opus5.5
---

Closed by reference 2026-10-09 (S379 conductor claude-opus5.5): fixed by yoke b6efa19 (ollama: keep serve off the model door; on origin/main), red/green unit tests in external/ollama; live genie probe on Dragon rc=0 after the fix (handoff section 3).
