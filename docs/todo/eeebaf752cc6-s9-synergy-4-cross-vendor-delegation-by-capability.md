---
id: eeebaf752cc6
kind: feature
title: 'S9 Synergy 4: cross-vendor delegation by capability - tool cmd run --capability deep-research'
seq: 1871
status: todo
priority: p2
labels:
    - yoke
created: 2026-10-11T03:52:01.134948Z
weave: 124
assignee: claude-opus5.5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed_by: claude-opus5.5
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. Capability-addressed calls building on Sprint 324 capability: and chat --capability: bashy tool cmd run --capability deep-research -- QUESTION selects a measured provider (claude, gemini, openclaw where measured), quota-aware via llmbudget. Remote variant: an agent on another sphere host is reached as a band request to that peer bashy llm door over the outpost mesh forwarder (Sprint 308 MVP proved outpost mesh service add llm plus mesh dial), not a new A2A plane; depends on 308 A3/A5 (advertise door, per-peer bearer/share). Acceptance: from a codex session it returns a report produced by another vendor; the selection is an S3 event; unmeasured commands are never seeded.

Manager partial live gate 2026-10-11 13:54 UTC: a genuine project-local Claude deep-research skill on claimed Dragon ran two successful WebFetch calls against RFC 4193/4291, with one measured host sample recorded only in a private matrix. A disposable trusted Claude TUI statusline bridge captured genuine five-hour 2% and seven-day 50% used quota; the host matrix and global trust/settings stayed unchanged. From a Codex caller, exact Bashy `fbc4aac5` and Yoke `53dfef0` selected `claude:opus5.5` with 0.5 remaining quota and one measured sample, emitted an S3 `capability-selection` event, and returned a 4,360-character RFC-cited report with exit 0. Typed outcome was success but verdict remained `unverified` because the per-command exec lacked a terminal stream-json success verdict. Sanitized `/tmp/s418-s9-quota-evidence.txt`; private auth/config/files were removed. This is genuine local selection evidence, not original-story acceptance: remote peer dispatch and share-policy A3/A5 in Sprint308 remain unchecked, and the verdict limitation must be preserved. No fake seed or new framework was added.

Manager 14:14 UTC pointed continuation: the same original Story-ID moves from umbrella into Yoke because `pkg/toolcmd` and `pkg/broker` own the remaining source. Do not create another story or broaden the declaration model. Read `/tmp/s418-s9-remote-audit.md` and current `pkg/toolcmd/capability.go`, `cmd.go`, `http.go`, and broker authorization before changing code. Add only the smallest peer candidate/door dispatch through the existing measured selection and typed HTTP paths, preserving local behavior, S3 selection event, secret-safe refusal, and unmeasured rejection. A raw owner-token HTTP overlay is not a per-peer share proof. Sprint308 A3/A5 advertise-door and bearer/share contracts are still todo: if those are required to dispatch safely, fail closed and submit a bounded source receipt with that exact prerequisite instead of bypassing access checks or inventing an A2A framework. Verify `GOWORK=off go test -short -count=1 ./pkg/toolcmd/... ./pkg/broker/...` plus focused fake-peer denial and positive tests; manager owns real two-host/account proof and final acceptance. No shared production token or hardcoded host. Do not touch active Cloudbox release or S17 service files.

Conductor continuation 2026-10-11 20:03 UTC: user orders a bounded live S9 recovery now. Sprint 308 A3 `7a5f32dbaafa` and A5 `53fde6fa2d0d` still show todo in the authoritative story files; verify current source and any deployed peer path before assuming they are absent. Own Yoke capability delegation/toolcmd files only. Implement the smallest safe real `--peer` dispatch and shared policy use if present, with deny-by-default for unauthorised/unknown peers; do not invent credentials, bypass sharing, or create a new framework. Use existing Outpost mesh API where available and focused fake-peer/real-peer gate as feasible. Preserve local selection behavior. Commit/push private Yoke source with original S9 trailers and submit exact pass/fail/gaps; manager owns integration and host claims. No false peer-proof from local selection.

Run `yoke#119` finished/merged its local-selection slice (`3a65ba5`) and was unlinked from the sprint card before this continuation; its source/history and partial receipt remain preserved. The old `blocked`/`resolution: fixed` frontmatter was reopened to `todo` because the original remote acceptance is still unmet. Do not redo its local work.

Manager claude-opus5.5 20:50 UTC: reopened after supervisor review (mb, 20:48). The 13:54 local acceptance evidence stands and is not to be redone, but the remote peer variant written in this original body is in scope and undelivered; it depends on Sprint 308 A3 7a5f32dbaafa and A5 53fde6fa2d0d (todo). --peer fails closed (3a65ba5, c61023c). Former todo 9ff3db74 is folded back into this story and removed. S9 stays open until a peer door dispatch under per-peer share policy returns another vendor's report from a codex session.
