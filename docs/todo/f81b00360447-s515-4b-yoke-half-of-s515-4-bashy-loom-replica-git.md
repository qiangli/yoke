---
id: f81b00360447
kind: feature
title: 'S515.4b yoke half of S515.4: bashy loom replica (Gitea push mirrors, sync on commit, to a peer loom) + bashy loom clone (url = nearest reachable replica, pushurl = primary, failover)'
seq: 2783
status: todo
priority: p0
labels:
    - loom
created: 2026-10-27T19:20:47.932059Z
assignee: claude-opus5
sprint: 515
sprint_id: e82160ea-95db-5f2a-b69e-b2d784b2996b
sprint_title: 'satellite loom: private forge + CI + groups (KISS/MVP, reuse loom/act_runner/scopes)'
resolution: fixed
closed_by: claude-opus5-n
---

Companion of S515.4 (umbrella 5955124e): one repo per run. Spec: umbrella docs/sprint-515-satellite-loom.md (S515.4; "So git read traffic is offloaded with standard git"); design of record docs/selvedge-self-hosted-git.md (one-way mirror primary -> replicas; NO symmetric multi-master).

KISS/MVP, reuse Gitea + git; invent nothing. Rule: bashy is all you need (bashy go/git/curl/dag; outpost ssh, never OpenSSH; never infer a missing tool from a bare-name probe).

ROUND 2 = LIVE ONLY. Code is merged (yoke 25a6d144: external/loom/replica.go — `bashy loom replica add|list|rm`, `bashy loom clone --replica`, `bashy loom remote refresh`). Reuse the reproducible live pattern from scripts/s515-live (S515.2 rounds 2-3): build bashy from mains on each host with `bashy go` (replace onto yoke main), scratch dirs + ports, stop processes after. Fix bugs found inside this story.

Code (external/loom, done):
1. `bashy loom replica add <replica-base-url> [--token ...]` on the primary: for every repo (or --repo owner/name), ensure the repo exists on the replica (Gitea API, same owner/org), then create a primary push mirror to it with sync_on_commit=true (Gitea API POST /repos/{o}/{r}/push_mirrors). Idempotent; `replica list`/`replica rm`.
2. `bashy loom clone <primary-repo-url> [--replica URL]...`: probe replicas in order (fast HTTP check), clone from the first reachable, then set remote.origin.url = that replica, remote.origin.pushurl = primary. `bashy loom remote refresh` re-picks url when the current replica is down (failover: next replica, then primary itself). Use bashy's git (pkg git / go-git) — no exec of a host git.
Unit tests with fake Gitea API + local bare repos (file://): replica ensure + mirror create idempotent; clone sets url/pushurl; refresh fails over.
Live (manager-coordinated hosts; ACCESS lines in `bashy claim list | grep s515`): primary = scratch loom on the Windows peer, replica = scratch loom on the staging satellite VM (Linux), client = a member host. Acceptance (spec): clone and fetch hit the replica (primary loom TX counter flat during fetch), push hits the primary, replica shows the new commit within seconds; stop the replica -> refresh fails over to primary.
Gate: go test ./external/loom; GOOS=windows go vet ./external/loom. No host names in this repo. Bounded 80 min.
Commit trailers: Sprint: #515 / Story: #2783 / Story-ID: f81b00360447.
