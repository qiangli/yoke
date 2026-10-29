---
id: 0e7c3ccb1411
kind: feature
title: 'S525.A6 yoke: shaped app kind + Genie request/preview/review'
seq: 2895
status: done
priority: p1
created: 2026-10-29T07:55:06.336699Z
weave: 677
assignee: claude-opus5.5
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T09:27:47.588516Z
resolution: fixed
closed_by: claude-opus5.5-a
---

Part of S525.A6 (umbrella story 0f0f7abad866). The shaped app kind + Genie request/preview/review experience, in yoke. Read first: ../docs/sprint-525-a0-decision-record.md sections D-A6, D-MVP, D-A4, D-A5 (private; parent dir of this repo; do NOT copy its text or describe internal mechanisms in public docs/commits — describe behavior only).
On main: pkg/shape (per-principal store/metadata; A7 is moving it to pkg/shape/state right now — import whatever path exists when you start and expect a rename at merge), pkg/shape/contain, webconsole principal resolution, unified unit model, `bashy delegate` (pkg/chat; headless: BASHY_DELEGATE_CALLER=genie BASHY_DELEGATE_ALLOW=codex:<m> bashy delegate --json --agent codex:<m> --timeout 20m --idle-timeout 5m --cwd <ws> "<task>", JSON envelope bashy-chat-v1). Running in parallel: S525.A2 (pkg/shape/verify, pkg/shape/evidence, pkg/src/pr.go) and S525.A7 (pkg/src versions/activation, pkg/shape/state, pkg/weave --base). Do not edit those.
Scope:
1. Shaped app kind in pkg/webconsole: a registered app of kind `shaped` whose public/ static assets are served from the requesting principal active revision tree (fall back to the base tree when no personal revision), and whose api/* handlers are Bash# units run per request under pkg/shape/contain with that principal data dir (BASHY_APP_DATA). No per-version long-lived processes. Absent principal => base app only.
2. Fixture app `notes` (synthetic data) under yoke testdata: list/add notes, two users.
3. Shape panel (plain HTML/CSS/JS in the launcher artifact, existing controls, no new deps/CDN): user types a request => a candidate workspace is prepared (an isolated clone of the principal current revision) => the shaping agent runs there via `bashy delegate` (record which agent did the work in the candidate metadata) => the result diff becomes a candidate commit => isolated preview of the candidate for that principal only => verify via a Verifier interface (A2 satisfies it; fake in tests) => user approves (calls an Activator interface; A7 satisfies it) or rejects (candidate cleaned up). Diff rendered server-side.
4. App content is untrusted input to the agent; the agent never receives the user secrets.
Acceptance (one focused red/green test): with fake delegate/verifier/activator, principal alice requests a change to notes, previews it, approves; bob still sees base notes; a rejected candidate leaves no workspace; anonymous gets base only. go test ./pkg/webconsole -run <your tests> green. Sprint/Story/Story-ID trailers; no hostnames.
