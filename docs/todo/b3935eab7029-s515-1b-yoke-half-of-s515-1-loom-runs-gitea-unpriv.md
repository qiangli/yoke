---
id: b3935eab7029
kind: bug
title: 'S515.1b yoke half of S515.1: loom runs Gitea unprivileged when started as root (satellite-local outpost is root; loom refuses root today)'
seq: 2784
status: done
priority: p0
labels:
    - loom
created: 2026-10-27T23:02:27.109002Z
weave: 642
assignee: claude-opus5
sprint: 515
sprint_id: e82160ea-95db-5f2a-b69e-b2d784b2996b
sprint_title: 'satellite loom: private forge + CI + groups (KISS/MVP, reuse loom/act_runner/scopes)'
closed: 2026-10-28T00:11:55.746003Z
resolution: fixed
closed_by: claude-opus5-r
---

Companion of S515.1 (cloudbox 731a1edd). Found live 10-27 on a fresh v0.18.41 product satellite (evidence cloudbox docs/s515-1-live-round4.md): the satellite-local outpost runs as root (it drives podman/systemd/DKS), its service monitor now starts the enabled loom unit, but loom exits: "loom: refusing to run as root (Gitea forbids it); run bashy as a regular user". So loom never serves on any product satellite.

ROUND 2 = PRE-FLIGHT (code merged, yoke main f8d584f7 incl. worker commit 5250fdf1): run the opt-in root integration test for real before the bashy release, on a Linux root venue that is not Dragon: noviwin1 WSL as root (manager claim host:noviwin1:s515-wsl-root has the ACCESS line: `bashy claim list | grep s515-wsl`). Use bashy there (`bashy go`, `bashy git clone` yoke + sh + coreutils into /tmp/s515-root-*), run `env LOOM_ROOT_INTEGRATION=1 bashy go test ./external/loom -run RootIntegration -v` as root, and also one real `bashy loom serve` start as root from the built bashy (go.work/replace onto the clones) proving Gitea runs as the unprivileged account (ps uid, /api/healthz 200). Then CLEAN UP: stop processes, userdel the created system account and remove its home/data, rm -rf /tmp/s515-root-*. Do not touch other dirs in that WSL (another lane's test trees live there). Fix bugs found inside this story. Bounded 45 min.

DECIDED (manager, 10-27): loom drops privileges itself when euid==0 on Linux (KISS; same behaviour for a root satellite outpost and a user-run home outpost; no outpost spec change, no user-data wrapper). Rule: bashy is all you need.
Do (external/loom):
- When root on Linux: ensure a dedicated unprivileged system account for loom (reuse an existing bashy/yoke helper if one exists; otherwise the minimal correct mechanism — document it), make loom's data dir (default ~/.agents/bashy/loom under the root HOME, i.e. on the satellite persistent volume) owned by it, and start Gitea with that uid/gid (SysProcAttr.Credential) and HOME/USER env + RUN_USER matching. The front proxy may stay in the parent process. Never run Gitea as root; do not set Gitea's unsafe-root override.
- Non-root: unchanged. Windows/macOS: unchanged.
- Idempotent across restarts/upgrades (existing data dir from a failed root start is empty or root-owned: fix ownership).
Red/green: unit tests for the decision + command construction (credential set when root, not when non-root; RUN_USER consistent); a root integration test if runnable in CI/podman elsewhere (not on Dragon: no podman). Live (manager arranges after release): the satellite-local outpost's loom serves.
Gate: go test ./external/loom; GOOS=linux go vet ./external/loom; GOOS=windows go vet ./external/loom. No host names in this repo. Bounded 60 min.
Commit trailers: Sprint: #515 / Story: #2784 / Story-ID: b3935eab7029.
