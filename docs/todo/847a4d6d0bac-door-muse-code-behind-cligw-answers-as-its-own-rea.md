---
id: 847a4d6d0bac
kind: bug
title: 'door: Muse Code behind cligw answers as its own read-only agent in an empty worker dir, so genie-muse-spark1.3 cannot drive a task'
seq: 19
status: done
priority: p1
labels:
    - door
created: 2026-09-30T11:33:41.135676Z
assignee: muse-conductor
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-08T19:45:58.530913Z
closed_by: claude-opus5.5
---

Steward genie trial 2026-09-30, yoke weave #127 (genie-muse-spark1.3 via door-muse-spark1.3, door freshly restarted): genie's first turn came back as a Muse Code agent reply, not a completion for genie's tool loop: 'Workspace is empty and session is read-only... Active workspace root cligw-worker-3776561295 has no go.mod... shell execution disabled (muse.bash denied) and file writes disabled... Requested Bashy tool is not in this session's tool list; only read-only search/read_file are available.' genie printed it and exited 0 after 1m21s (weave state no-op, 0 commits). The identity is launched as 'cligw-pure-completion', but for muse the CLI's own agent persona and tools leak through. genie-opus5 through the same door did work (#128). Fix (KISS): make muse's door launch a pure completion like claude's (no tools, no agent persona; the prompt is the conversation), or mark door-muse not completion-capable so genie refuses it at launch. Red/green: a genie tool-call turn via door-muse returns a tool call/text completion, not a Muse agent report.


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

Conductor scope (2026-10-08): this run covers the door-muse trio: 847a4d6d0bac (Muse Code behind cligw answers as its own read-only agent in an empty worker dir), 3c5bbbb60f9e (drop the muse tool-calling refusal so genie-muse-spark1.3 answers; live pong gate), 2729b0108b63 (genie on door-muse makes no tool calls). Code: yoke pkg/cligw (muse binding, worker dir), fleet muse tool yaml. Prove live on Dragon with the muse subscription seat; tests pin the behaviour. Close the other two todos by reference in your report.

Worker rules (Sprint 379 conductor, 2026-10-08): work in the submodule named above; commit and push INSIDE it to its default branch (public repos: main), then stop - the conductor bumps the umbrella pin. Every commit carries Sprint: #379 plus Story/Story-ID trailers for this story, with BASHY_AGENT set to your own binding. Reproduce first with a red unit test, fix at the root, never skip, quarantine or add to a known-failures list. This dev box (Dragon) is for builds and focused unit tests only. Windows runs go to noviwin1.local, Linux runs to the novidesign.local podman machine bashy or the repo CI, and macOS full suites to novidesign.local (all passwordless ssh; use the .local names). Gate before push: go build ./... and go vet on the module, plus the focused tests, with the exit code captured (never gate a push on a pipe). Report: the root cause, the commit SHA, the exact tests run and where, and any CI run URL.
