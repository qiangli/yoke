---
id: f76a35b1a290
kind: bug
title: S499.18 DKS reattach between hosts of the same account fails 403 ('host must be owned by or shared with the paired account')
seq: 2767
status: todo
priority: p0
created: 2026-10-26T02:35:51.334094Z
sprint: 507
sprint_id: 77d7ab0f-6aef-5bd3-876d-3f50e9b585ea
sprint_title: 'YC demo blockers: four bug fixes (dks reattach, executor image default, AMD VRAM on Windows, kubectl /c/ paths)'
---

Refined 2026-10-25 (demo-critical, cluster segment). With the executor image fixed (ghcr.io/qiangli/bashy-scratch:v1.0.0-rc.15-dev, public), a session from Dragon runs on puppy-3a07cd18 and a one-shot job prints its output and exits 7. Reattach fails in two layers: (1) yoke external/dks/userland_session.go pairedAuthHeaders sends the paired Outpost's host token (agent.json access_token); zoo answers 403 'host must be owned by or shared with the paired account' from elephant AND from puppy itself (same gmail account). (2) With an owner token (DHNT_OWNER_TOKEN exchanged on zoo via POST /api/v1/tokens/exchange, scopes host:proxy ssh:read) passed as OUTPOST_SESSION_JWT, auth passes but the dial /matrix/h/puppy/app/vk-user-<ns>-bashy-run-<id>-<hash>/attach returns 404: the session app is not published on the executor host (puppy outpost rc.15). Acceptance: a session started from one device reattaches from another device of the same account with full replay and exit code, with no manual token. Cross-cutting: satellite ownership (S498) + outpost vk app publishing; coordinate with Lane 2.
