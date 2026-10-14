---
id: c6fdc1d27a2a
kind: task
title: 'S434 OpenClaw CLI: pinned npm integrity and working feed declaration'
seq: 2025
status: assigned
priority: p1
created: 2026-10-14T07:25:20.213403Z
weave: 253
assignee: muse-conductor
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
---

You are the implementation worker continuing S434 yoke run250, story c6fdc1d27a2a (#2025). The manager REJECTED archive-only installation. Prior resume failed to read the review and only added a cwd test. This is NOT DONE. Read current code; preserve existing useful pin/declaration work. Fix the concrete blocker: npm openclaw2026.10.2 declares65 dependencies but only3 are bundled. binmgr.Ensure extracting package/openclaw.mjs does not install production dependencies; Windows cannot directly exec mjs. Use existing external/node/npm tooling to install the production graph from the integrity-verified pinned npm archive and execute with Node cross-platform. No new installer framework, no gateway/service. Add small meaningful tests and show actual provisioned CLI help/version works (no messages). No novidesign access while conformance is active; local short tests allowed, heavy installation smoke can be prepared for manager. Read weave comments250 and sprint fail note. Commit code with Sprint434 Story2025 Story-ID c6fdc1d27a2a and your actual worker BASHY_AGENT. No pin edits to consumers, no main push. 25m cap. Do not submit until missing production dependencies/Node launch are fixed; if impossible identify exact blocker. This prompt overrides the original issue brief where they differ. Recover prior useful code: git fetch /Users/qiangli/projects/poc/dhnt/yoke refs/salvage/abandoned-250 then cherry-pick d5a51b22 and d878bb06; skip KB-only auto commit ee9734c2. Preserve this work, no rewriting from scratch.
