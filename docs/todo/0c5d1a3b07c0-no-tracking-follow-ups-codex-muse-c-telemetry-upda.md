---
id: 0c5d1a3b07c0
kind: chore
title: 'No-tracking follow-ups: codex/muse -c telemetry+update overrides in pkg/cligw; notrack env cleanup'
seq: 2002
status: done
priority: p1
created: 2026-10-12T18:00:39.002771Z
assignee: claude-sonnet5.5
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-12T18:39:40.205846Z
closed_by: s434-manager
---

Sprint 434 rule (2) no third-party tracking: consolidated follow-ups from accepted stories (yoke main bfdead7b, df246d32).
(1) MAIN: inject per-launch -c config overrides that env cannot express (verified against pinned binaries by the ba2fbf33 worker): codex 0.162.0 -c check_for_update_on_startup=false -c otel.exporter=none (statsig is a vendor endpoint; pinning none stops an operator config.toml re-enabling it); muse 1.4.4 -c telemetry.enabled=false (muse rejects unknown -c keys, this one is valid). Natural home: the same insertAfter(args,"exec",{"-c",...}) used for web_search="disabled" in yoke pkg/cligw/worker.go; update pkg/cligw/muse_binding_test.go pins. Do NOT edit pkg/binmgr or pkg/fleet Go (S433). agy 1.3.2 has nothing to inject.
(2) external/notrack cleanup: Vars has both NPM_CONFIG_* and npm_config_* keys colliding in the case-insensitive wantUpper map (map-order dependent); keep one form. Comments claim DO_NOT_TRACK/CHECKPOINT_DISABLE cover rclone/helm/mise/doctl update checks; upstream does not document that - cite or drop.
Acceptance: unit tests assert codex and muse argv carry the overrides; notrack test deterministic; go test ./pkg/cligw/... ./external/notrack/ green. Commit in yoke with Sprint/Story/Story-ID trailers, then sprint submit.
