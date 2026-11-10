---
id: 3e59a2b94f26
kind: doc
title: 'S606/S583 W0 Public wire contract: cloudbox members (circle, mutual accept, reverse view) and pair-once + satellite attach'
seq: 3079
status: assigned
priority: p0
labels:
    - wire
    - satellite
    - members
    - satellite-cli
created: 2026-11-10T04:36:32.685479Z
assignee: claude-opus5.5
sprint: 606
sprint_id: ebee3343-b774-520a-9d53-b57b306f4210
sprint_title: Streamline Cloudbox master, satellite, and outpost relationships
---

Wave 0 of the S606/S583 plan (2026-11-09). Deliver docs/cloudbox-members-attach-wire.md (one line in the docs index if present): the PUBLIC wire contract that the private cloudbox tickets (members a99e88ac, attach 988ccd26) implement and that yoke/outpost/bashy clients code against with httptest fixtures. Request/response JSON, status codes, Bearer scopes, idempotency, error envelopes, cache/TTL and revocation timing. No private internals.
Cover: GET/POST/PATCH/DELETE /api/v1/members (circle field), mutual accept POST /api/v1/member/invitations/:id/accept (accept adds the reverse row; operator decision), GET /api/v1/members/of; master identity-only pairing (exchange that stores a host record, no tunnel); POST/DELETE /api/v1/satellites/:id/attach (host Bearer -> attach_token); satellite POST /api/v1/hosts/attach; host Bearer satellite list; DELETE /api/v1/hosts/self; GET /api/v1/satellites/:id; host->satellite assignment over Bearer (S5).
Map each endpoint to the bashy verb that drives it (agent-first, everything through bashy): `bashy member ...`, `bashy outpost satellite attach|detach|list`, `bashy satellite show`, `bashy paired-host ls --satellite`, `bashy llm peer --email/--circle`. A step with no verb is a gap to fix, never an ad hoc HTTP call.
Also ship the fixture: a small yoke httptest fake (pkg/fleet or a testdata package) serving this contract so Wave 1-3 client stories need no live cloudbox.
Acceptance: doc reviewed by the operator and the cloudbox ticket owners; fixture compiles with a smoke test; S583.1 design references it.
