---
id: 6b49bcb348e4
kind: task
title: S531.D4w llm env --via mesh in a WSL distro holding its own grant fails (no outpost in WSL); must use the Windows outpost
seq: 2930
status: done
priority: p1
created: 2026-10-29T18:44:33.472203Z
assignee: claude-sonnet5.5
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T20:15:58.529078Z
closed_by: s531-manager
---

Found in D4/D3 live 10-29 on elephant. After 'bashy llm door import' in the WSL distro (its own grant), 'bashy llm env --from dragon --via mesh' fails: 'mesh forward to dragon: outpost mesh consume ls: exec outpost: not found'. WSL's outpost is the Windows one. A WSL distro with no grant works, because it goes through wslEnvFrom interop (Windows bashy binds the forward on the vEthernet address). Fix: in WSL, when the mesh route needs outpost and none is on PATH, open the forward through Windows interop (bashy.exe llm env --bind-for-wsl, or outpost.exe consume add --local <gw>:port) while keeping the WSL grant's credential; or prefer the Windows store for mesh. Red/green unit test via the wslInterop/outpostRun seams; go test ./pkg/broker.
