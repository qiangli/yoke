---
id: 887a07a124ba
kind: bug
title: 'S537.S1 shaped apps can declare a server: console builds + runs the principal''s revision (desk tools become shapable)'
seq: 2949
status: done
priority: p1
labels:
    - desk
created: 2026-10-30T17:38:45.905815Z
assignee: claude-opus5.5
sprint: 537
sprint_id: 3ec79210-039c-57d7-a0b5-7abfeeefbb1c
sprint_title: 'bashy desk: agent-human tool suite (notepad, form, chart, whiteboard, brainstorm) on one contract + UI kit'
closed: 2026-10-30T19:51:15.502644Z
closed_by: s537-manager
---

Gap found in S537 (contract point 6 "shapable"; plan pre-authorizes shaping gaps as in-sprint bugs). Today a shaped app (`bashy app add NAME --type shaped --source DIR`) is served by the console only as public/ assets + per-request api/<name>.md Bash# units (yoke/pkg/webconsole/shaped.go serveTree/serveAPI), and type shaped forbids a port (yoke/pkg/fleet/apps.go). Desk tools (bashy/examples/desk/<tool>, also whiteboard/brainstorm) are a Bash# front + a long-running pure-Go server, so they cannot get src history/preview/restore/undo.

Fix (MVP, decided by S537 conductor): a shaped app may declare a server in its source tree (one small manifest file at the app root, e.g. `shaped.json` {"server":{"build":"go build -o $OUT .","dir":"server-or-.","args":["serve","-addr","$ADDR","-data","$DATA"]}} — exact shape is yours, keep it minimal and documented). For a request from principal P, the console resolves P's active revision (existing logic), builds the server from that commit's tree into a cache keyed by commit SHA (GOWORK=off, CGO_ENABLED=0, -trimpath), starts it lazily on an ephemeral 127.0.0.1 port with a per-principal data dir (reuse the dataDir the units get), proxies the request (prefix-stripped, same headers/no-store rules as serveTree), and stops idle servers after a timeout. Apps WITHOUT the manifest behave exactly as today. Containment: run the server under the same contain/effects policy shaped units use (yoke/pkg/shape/contain); never bind non-loopback. History/preview/restore/undo are unchanged (they act on the source tree).

Acceptance (focused, red/green): unit tests in yoke/pkg/webconsole + pkg/fleet using a tiny fixture Go server app (testdata) — (a) red today: shaped app with server manifest is not served / is rejected; green after; (b) two principals on different revisions get responses from their own revision's server; (c) restore/undo switches the served binary; (d) manifest-less shaped app (existing notes fixture) unchanged; (e) build failure returns a clear 5xx with the build error, never serves a stale other-principal binary. Then prove on a real desk tool: `bashy app add notepad --type shaped --source <abs>/bashy/examples/desk/notepad` (add the manifest to notepad in the same story) and `bashy src history notepad --user P` works. Dragon: focused go test only, no podman. Do not touch cloudbox. Spec: $HOME/projects/poc/dhnt/docs/sprint-537-bashy-desk-plan.md; shaping design: $HOME/projects/poc/dhnt/docs/software-shaping-epic.md.
