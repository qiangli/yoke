---
id: 079089a4bed8
kind: task
title: S531.D4w2 WSL own-grant mesh forward reuses a persisted ':0' consume (base http://<gw>:0, every call times out)
seq: 2937
status: done
priority: p0
created: 2026-10-29T22:04:35.164114Z
assignee: claude-sonnet5.5
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T22:27:07.955598Z
closed_by: s531-manager
---

Found in the D3 WSL regression run 10-29 21:39Z on bashy 75908e49, elephant. WSL holds its own grant (D4w path, no outpost in WSL). The first 'llm env --from dragon --via mesh' worked (base 172.27.208.1:60410, D4W-OK at 20:15Z). Later calls resolved base http://172.27.208.1:0/v1 and every chat request timed out ('dial tcp 172.27.208.1:0: i/o timeout'), so genie never ran. Cause: yoke 78551f6f ensureMeshForward's Windows path adds the consume with --local <gw>:0 (WSL cannot pick a port on the Windows side). outpost persists the requested ':0' (D4o noted ephemeral binds keep the requested addr), and the reuse loop then matches that entry and returns '<gw>:0'. Fix: never reuse an entry whose port is 0 (and remove such stale entries); for the Windows path, request a concrete port (e.g. a deterministic high port derived from the service+peer, retrying the next on 'address in use') so the persisted addr is the real one. Red/green unit tests via the outpostWinRun seam: a ':0' entry is not reused; a second call reuses the concrete port. go test ./pkg/broker only.
