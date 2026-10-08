---
id: 3c5bbbb60f9e
kind: bug
title: 'door-muse: drop the muse tool-calling refusal so genie-muse-spark1.3 answers (live pong gate)'
seq: 25
status: done
priority: p1
labels:
    - door
created: 2026-09-30T16:16:34.75947Z
assignee: claude-opus5.5
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-08T19:46:00.738434Z
closed_by: claude-opus5.5
---

Follow-up to 847a4d6d (failed live 2026-09-30, 3 pts max). Steward live check: bashy genie -m door-muse-spark1.3 'reply with the single word: pong' -> rc 124 after 200 s. cligw usage.jsonl: every muse request from 09:03 PT has latency_ms 0 / 0 tokens = the 847a4d6d fix-(b) 400 refusal (yoke 9605bfe, ToolCallingCapable(muse)=false), which the broker retries with backoff (door.log 09:08:22..09:09:58; llm pools genie-muse-spark1.3 W=59.11s). genie always offers its tool, so a refusing seat can never serve genie. Conductor evidence: muse exec with cligw's tools-off flags (--no-session-log --no-foreign-personal-context --disable-web-tools --disable-shell --disable-write) and cligw's toolInstruction prompt returns a correct envelope {tool_calls:[{name:bashy,arguments:{script:ls -la}}]} and answers pong. Fix (KISS): remove the muse refusal (keep the tools-off flags); if the original persona leak reproduces with genie's real system prompt, fix it in the muse prompt rendering only. ACCEPTANCE GATE (live, conductor-run after bashy dag install + bashy llm down/up): (1) bashy genie -m door-muse-spark1.3 'reply with the single word: pong' prints pong within 120 s; (2) in a scratch git repo, genie -m door-muse-spark1.3 'run exactly: mkdir -p d && touch d/probe.txt' creates d/probe.txt. Unit tests stay green.

Recovery correction contract (21:28Z): 3pt/12m, actual s340-muse-triage-astra. Source30366b67 already removes refusal; prior real genie file probe still returns persona refusal and no tool call. Inspect current Muse CLI capabilities without an API probe and existing sanitized genie request evidence. Fix only Muse completion prompt/argv rendering to treat supplied conversation as the completion context and emit caller-owned tool envelope while retaining tools-off isolation. Add meaningful red/green render fixture from actual request shape; preserve Claude/Codex behavior. Do not claim live success from unit tests. No native persona workaround that enables local shell/write. No install, service restart, push, pins or manager mutations; conductor gates source and owns live tests after shared install coordination. If native system semantics cannot be made reliable within budget, preserve evidence and report exact blocker; no unsupported success. Commit Sprint340 Story25 Story-ID3c5bbbb60f9e with actual worker identity.


Conductor live qualification, 2026-09-30 23:12 UTC: source candidate 8430b4aa
(managed yoke 5a0fccd9) passed independent unit/targeted-race/vet and full
pre-push checks. Consumer 35a3aef1 was published, installed through the DAG,
and its four launcher/payload hashes matched the verified build. After the
required model-door restart, both live probes failed: pong exited 124 after
120.010 seconds; exact mkdir/touch exited 124 after 120.010 seconds and the
probe file was absent. Raw evidence remains in the sprint's shared evidence
record. This story stays OPEN; source tests are not a live-tool pass. A bounded
read-only diagnosis is checking the gateway/client path, with no additional
model probes or source changes authorized by this note.
