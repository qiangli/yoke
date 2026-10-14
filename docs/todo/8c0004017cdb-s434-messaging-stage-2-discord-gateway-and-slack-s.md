---
id: 8c0004017cdb
kind: task
title: 'S434 messaging stage 2: Discord Gateway and Slack Socket Mode inbound'
seq: 2024
status: wontfix
priority: p1
created: 2026-10-14T07:25:20.087151Z
weave: 251
assignee: codex-gpt6-sol
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-14T07:57:01.967727Z
---

Continue accepted 48912cbda346 stage 1. Scope from docs/sprint-434-master-execution-plan.md B: Discord Gateway and Slack Socket Mode inbound feeding existing envelope/pairing/approval/photo/bridge path. KISS: extend existing adapters, no new abstraction or unrelated feature. Focused offline tests for protocol events, reconnect/cancel, ack and duplicate handling, paired/unpaired and photos/approve. Keep secrets out of logs. No real-account messages without manager coordination. Read yoke CLAUDE.md. Work in isolated yoke weave only; do not edit decl.go/openclaw.yaml/binmgr or dependency pins (separate lane). No heavy builds Dragon; short focused unit tests allowed. Deliver commit with Sprint/Story/Story-ID trailers and exact tests. Manager independently gates and integrates. Deadline within 45m; report blockers promptly.

SUPERSEDED DUPLICATE: original story48912cbda346 explicitly owns Stage2. All work and evidence are carried there; this closure is a tracking correction, not a passing delivery or scope addition. Preserve linked worker commits and test evidence.
