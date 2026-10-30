---
id: 557d8c199396
kind: task
title: 'native git engine (yoke/git): ''git init -q'', ''git -c k=v ...'', ''git log --oneline'' unsupported on hosts without host git'
seq: 2939
status: done
priority: p1
created: 2026-10-29T22:29:34.978279Z
assignee: agy-gemini3.8-flash
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T23:55:12.327956Z
closed_by: s531-manager
---

Moved from bashy c8d1da71 (the engine is yoke/git, Exec handlers). Found in the D3 live run 10-29 on elephant WSL, where host git is absent: 'bashy git init -q .' -> unknown shorthand flag 'q'; 'bashy git -c user.email=.. -c user.name=.. commit -qm init' -> '-c is not applied by bashy native git engine'; 'git log --oneline' -> not served by the native engine. Agents (genie) then cannot commit in a fresh repo. Same-on-every-OS, zero setup: support -q on init/commit, -c config overrides, log --oneline in the native engine. Red/green tests; focused go test.
