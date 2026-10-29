---
id: 97951b72d90f
kind: bug
title: 'S524.3r yoke: cloudbox adapter reads real profile fields (live-check fix)'
seq: 2905
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T08:59:31.272261Z
weave: 687
assignee: agy-gemini3.8-flash
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T09:31:11.803444Z
resolution: fixed
closed_by: agy-gemini3.8-flash-k
---

Part of S524.3 (umbrella story e9b678ee1a02, Sprint 524): live-check fix for pkg/accounts/adapters/cloudbox. A read-only live Discover on a real host found 2 real profiles but BOTH unresolved with 0 satellite links, because the adapter does not read the fields real files carry:
- Real outpost config files (top-level <matrix>/agent.json and profiles/<name>/agent.json) carry `server_addr` (+ `server_port`, `protocol`) and `agent_name`; the profile's one-line `server` sidecar is optional (the active top-level agent.json has none). Derive the endpoint from the sidecar if present, else from protocol/server_addr/server_port in agent.json.
- `outpost status --json` returns an object with top-level keys `config`, `state`, `status` (nested), not a flat `user` field. Read identity fields from those nested objects (inspect outpost/cmd/outpost/status.go in the sibling outpost repo, read-only, for the exact JSON field names; do not import outpost).
- Identity: a cloudbox host pairing is a real provider identity: key = (endpoint, agent_name) when no user is known, recorded as Resolved with Username empty and Name = agent_name; when a user is known, (endpoint, user) as today. Keep unresolved only when neither endpoint nor agent_name is known.
- Satellites: a profile whose endpoint differs from the default/active cloudbox endpoint is a satellite instance of the same user/host; link it via Links.Satellites on the account for the active endpoint ONLY when the agent_name matches; otherwise keep it its own account. Do not invent links.
- NEVER read or copy token-like fields (token, access_token, admin_session_key, mcp_bearer_token, files_signing_key, *_key, cookie, password) into any record; extend the sentinel test with fixture files shaped like the real ones (placeholder values; hosts owl/otter, example.com endpoints).
Acceptance: fixtures shaped like real files (top-level agent.json without sidecar + one profile with sidecar) yield Resolved accounts with correct endpoints and agent_name; two endpoints stay separate; sentinel tokens absent from records/JSON/errors. Gates: go test -race ./pkg/accounts/..., go vet, GOOS=windows go build ./pkg/accounts/... . Do not edit pkg/accounts/cmd.go or registry files (another run is changing them). Trailers: Sprint: #524, Story: S524.3, Story-ID: e9b678ee1a02 (plus this yoke story id).
