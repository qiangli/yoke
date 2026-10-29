---
id: 5d7b10835124
kind: feature
title: 'S525.A4 yoke: web principal and per-user app state'
seq: 2890
status: todo
priority: p1
created: 2026-10-29T07:16:11.144825Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Part of S525.A4 (umbrella story 3f0ae9835bca). Web principal and per-user app state in yoke. Read first: ../docs/sprint-525-a0-decision-record.md sections D-A4 and D-MVP (private; parent dir of this repo; do not copy its text here).
Scope:
- pkg/webconsole: resolve an authenticated web principal independently of the OS user running the server: cloud-vouched user, host owner (session/loopback), and paired devices. Pairing approval gains an operator-chosen named principal (e.g. `bashy app pair approve <ticket> --principal NAME`) so one host can serve two distinct web users. Absent or ambiguous principal fails closed. Proxied apps get the principal only via a launcher-set header; inbound copies are stripped.
- New pkg/shape: per-(app, principal) state record (active revision, last-known-good, adopted items, notices) with atomic write-rename, and a private data directory per (app, principal) kept outside any source tree (exposed to the app as BASHY_APP_DATA). Versioned feature metadata reader (origin, base app identity, revision, requires/conflicts, scope, schema range) with incompatible-combination detection.
- Do NOT edit pkg/src/manifest.go (S525.A5 owns it) or pkg/skills (S525.A1).
Acceptance (one focused red/green test): two principals on one httptest launcher see only their own state/data; forged principal header is ignored; absent/ambiguous principal => base only, no shaping; incompatible features detected. go test ./pkg/webconsole ./pkg/shape green. Describe behavior only in docs/commits; no hostnames. Sprint/Story/Story-ID trailers.
