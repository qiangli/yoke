---
id: b875998660f4
kind: bug
title: 'S525.A5d yoke: strict app data boundary — units read/write only own app data; cross-app only via owner API; denial table on every OS'
seq: 2932
status: done
priority: p0
labels:
    - windows
    - linux
created: 2026-10-29T19:07:47.47288Z
weave: 720
assignee: claude-opus5.5
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T19:36:13.852011Z
resolution: fixed
closed_by: claude-opus5.5-r
---

Operator decision 2026-10-29 (unit write-root question, option A, made stricter). Rules, enforced by bashy the same on every OS, with no sandbox required: (1) A unit may write only its own app's data for the current principal (BASHY_APP_DATA). (2) Direct access to another app's data is BANNED, write AND read: apps always interact through the owning app's API (routes/units) as the same principal. Data location is the owner's business (local disk, S3 behind a local API, a mount), and callers never assume local storage. (3) The owning app manages its own rollback/cleanup through its API and version history. (4) Writes outside app data (e.g. the Files surface writing ~/Documents) need an effect declared in the app's own Bash# and granted by the user at adoption, revocable; a candidate can never widen its own grants (A2). (5) Running a non-bashy external program needs a declared effect or a @sandbox island (bashy oci); Bash# redirections and bashy builtins are mediated by the interpreter. (6) Other users' data, BASHY_HOME/bashy state, verifier/evidence and protected paths are never writable, whatever the grant. Tests (each must be denied): write into another principal's data; read or write another app's data directly; write outside granted locations; a candidate widening its own effects; an undeclared external program. Must succeed: a cross-app call through the other app's API as the same principal. Identical on native Windows and Linux (elephant). Restores the preview-escape guarantee that A5r's sandbox removal dropped. Master plan docs/software-shaping-epic.md.
Umbrella goal story: e4100a380606 (goal a5d). Scope (operator: keep it small): an interpreter-level read/write path boundary on the EXISTING guard system (pkg/shape unit runner / effect caps; reuse, no new isolation layer, no sandbox), plus the cross-app API call path (unit calls another app's route/unit as the same principal). Allowed write root = BASHY_APP_DATA for (app, principal); reads also confined to own app data + declared/granted effects; never-writable set = other principals' data, BASHY_HOME, verifier/evidence, protected paths. Undeclared external programs refused (declared effect or @sandbox island only). Denial table test (table-driven, same on every OS, no POSIX-only helpers): other principal write; other app read; other app write; write outside grants; candidate widens own effects; undeclared external program -> all DENIED with reason; cross-app API call as same principal -> OK. Gate: go test ./pkg/shape/... ./pkg/src; ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair'; GOOS=windows vet.

STEER 2026-10-29 ~19:20Z (operator via s525-manager; overrides anything above that conflicts): "API" = any commonly agreed interface — CLI flags/args, a Bash# unit call, or a declared route; NOT necessarily REST. App data is private by default; cross-app access only through the owning app's interface, as the same principal. Implement ONLY the data boundary plus the cross-app call path. NO events, notifications or cascade logic (deferred to Sprint 535). Details: latest operator-shaping-1029 decision comment on the sprint 525 card.
