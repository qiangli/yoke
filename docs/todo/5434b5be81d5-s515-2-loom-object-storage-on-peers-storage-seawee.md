---
id: 5434b5be81d5
kind: feature
title: 'S515.2 loom object storage on peers: [storage] -> seaweedfs S3 on a peer with SERVE_DIRECT (LFS, packages, Actions logs/artifacts, archives); spike stable name <peer-id>.<sat-id>.localhost + fixed outpost port routed to the peer''s s3'
seq: 2775
status: doing
priority: p0
labels:
    - loom
created: 2026-10-27T13:53:05.888294Z
assignee: claude-sonnet5.5
sprint: 515
sprint_id: e82160ea-95db-5f2a-b69e-b2d784b2996b
sprint_title: 'satellite loom: private forge + CI + groups (KISS/MVP, reuse loom/act_runner/scopes)'
resolution: fixed
closed_by: claude-sonnet5.5-d
---

S515.2 loom object storage on peers. Spec: umbrella docs/sprint-515-satellite-loom.md (S515.2, "Stable peer storage name"). Code is DONE and on main: yoke 259a9d82 (loom [storage] + SERVE_DIRECT, LOOM_S3_* env; docs/peer-store-name.md), outpost 997b8a3 (store router 127.0.0.1:31890, Host-routed to the peer's s3 mesh service). This round = LIVE acceptance only; fix bugs you find inside this story.

Rule: bashy is all you need. Use bashy's own tools on every host: `bashy go` (Go 1.27.2 toolchain), `bashy git`, `bashy dag`, bashy's seaweedfs / loom builds, `outpost ssh` (never OpenSSH). Never conclude a tool is missing because a bare name (`go`, `git`) is not on PATH; use the bashy form.

Host (claim held by the manager): the big peer, reached with `outpost ssh <user>@<peer>.local:2222 -- <cmd>` (Windows; host name and user are on the private sprint card and in the manager's message to you; do NOT write them into this repo). Heavy work (clone, build) runs there, never on the dev box.

Steps:
1. On the peer: `bashy git clone` bashy, yoke, outpost, sh (public repos) into a scratch dir; build bashy and outpost from current mains with `bashy go build` (GOWORK as the umbrella go.work or a small go.work over the clones).
2. Run seaweedfs S3 (bashy-built) on the peer; create a bucket and keys.
3. Run loom (`bashy loom serve` from the new build) with LOOM_S3_PEER/LOOM_S3_SAT (or LOOM_S3_ENDPOINT) + keys, so [storage] points at <peer-id>.<sat-id>.localhost:31890; run the new outpost so its store router serves :31890 -> local s3.
4. Acceptance: push a container image to loom packages and pull it back, and upload/download an Actions-style artifact or LFS object; show (a) the download is a 30x redirect to the store name and the bytes come from seaweedfs, (b) loom's process tx byte counter stays flat for those bytes (before/after numbers).
Report commands + outputs. Limitation to state: single-host proof (loom and store on the same peer); cross-host comes with S515.4.
Bounded: stop and submit after 70 min. Commit trailers (only if you fix code): Sprint: #515 / Story: #2775 / Story-ID: 5434b5be81d5.
