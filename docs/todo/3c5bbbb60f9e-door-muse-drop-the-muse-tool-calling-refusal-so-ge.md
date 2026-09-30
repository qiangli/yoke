---
id: 3c5bbbb60f9e
kind: bug
title: 'door-muse: drop the muse tool-calling refusal so genie-muse-spark1.3 answers (live pong gate)'
seq: 25
status: todo
priority: p1
labels:
    - door
created: 2026-09-30T16:16:34.75947Z
sprint: 340
sprint_id: 07abaf0d-4c54-57e7-b324-8c926b4c300f
sprint_title: Pure-Go m4, localedef and lp; listing view for the optional external POSIX tools
---

Follow-up to 847a4d6d (failed live 2026-09-30, 3 pts max). Steward live check: bashy genie -m door-muse-spark1.3 'reply with the single word: pong' -> rc 124 after 200 s. cligw usage.jsonl: every muse request from 09:03 PT has latency_ms 0 / 0 tokens = the 847a4d6d fix-(b) 400 refusal (yoke 9605bfe, ToolCallingCapable(muse)=false), which the broker retries with backoff (door.log 09:08:22..09:09:58; llm pools genie-muse-spark1.3 W=59.11s). genie always offers its tool, so a refusing seat can never serve genie. Conductor evidence: muse exec with cligw's tools-off flags (--no-session-log --no-foreign-personal-context --disable-web-tools --disable-shell --disable-write) and cligw's toolInstruction prompt returns a correct envelope {tool_calls:[{name:bashy,arguments:{script:ls -la}}]} and answers pong. Fix (KISS): remove the muse refusal (keep the tools-off flags); if the original persona leak reproduces with genie's real system prompt, fix it in the muse prompt rendering only. ACCEPTANCE GATE (live, conductor-run after bashy dag install + bashy llm down/up): (1) bashy genie -m door-muse-spark1.3 'reply with the single word: pong' prints pong within 120 s; (2) in a scratch git repo, genie -m door-muse-spark1.3 'run exactly: mkdir -p d && touch d/probe.txt' creates d/probe.txt. Unit tests stay green.
