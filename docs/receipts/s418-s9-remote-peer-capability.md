# S418 S9 — remote peer capability dispatch receipt (pointer)

2026-10-11 23:02–23:12 UTC. Full sanitized report: `/tmp/s418-s9-remote/final/REPORT.md` on the door host (operator-local, not published).

- A real Codex 0.162 session on a throwaway paired outpost peer ran
  `bashy tool cmd run --capability deep-research --peer <door> --json -- "<RFC 4193 question>"`.
  Its own model turns and the `--peer` dispatch both used only the per-peer door credential.
- A private door (its own HOME/BASHY_HOME/CLAUDE_CONFIG_DIR, reached through an outpost mesh dial) authenticated
  `peer:<name>` under `share: allow`. It then selected `claude:opus5.5` from one genuine host measurement (no seed) and a genuine
  Claude statusline quota snapshot (remaining 0.46), and returned a Claude report. That report was RFC 4193 §3.1-cited, with verdict
  `succeeded`.
- The caller emitted the S3 `capability-selection` usage event with `peer=<door> binding=claude:opus5.5`.
- Negatives on the same credential: `share deny` → 403, `revoke` → 401.
- Teardown: the door was stopped, the credential revoked, the peer lane stopped, and its files and the private root shredded.

Acceptance is the manager's decision.
