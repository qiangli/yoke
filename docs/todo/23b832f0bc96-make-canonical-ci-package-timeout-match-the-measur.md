---
id: 23b832f0bc96
kind: test
title: Make canonical CI package timeout match the measured serial weave suite
seq: 40
status: done
priority: p2
labels:
    - ci
created: 2026-10-11T00:17:44.788625Z
weave: 66
assignee: codex-gpt6-sol
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-11T00:56:44.090904Z
closed_by: s401-manager
---

Necessary Sprint401 gate follow-through: full unchanged CI scope passes5387 top-level tests/0failures under an explicit external45m package bound. Actual serial pkg/weave measured950.18s (~16min), exceeding Go default10min on Linux; Windows package selection excludes weave, so this does not claim a weave timeout on every OS. Make the canonical scripts/ci-test-gate.sh primary go test invocation explicitly bounded45m so CI matches measured gate. Scope only that flag plus explanatory comment; preserve package selection, confirming reruns, known-failure baseline, all assertions/JSON extraction/exit logic. No productionGo/UI/timeout grace changes. Implementer must use isolated yoke weave, selfclaim and selfsubmit Story9999 canonical --repo /Users/qiangli/projects/poc/dhnt/yoke, with true injectedAgent and Sprint/Story/Story-ID trailers. Dragon source/shellsyntax only; no full Go/DOM/builds. Root s401-manager owns nativehosts and reruns canonical gate WITHOUT anyGo wrapper on Linux. Bound5min,1point; independent root review and actual native gate before accept. Public story: no private umbrella evidence, secrets or proprietary code. Commit source insideyoke, no pushes/canonical edits. Preserve injected trackednotes beforecommit. Root publishes after verification.


Independent delivery record (2026-10-11): original Fable5.1 commit0055c87 supplied the primary45m flag. Actual Codex gpt6-sol final correctionb97860f restored injected tracked notes and corrected timing/scope comments, then selfclaimed/selfsubmitted. Root s401-manager independently graded/reviewed and normally merged weave66 at1102513. Direct canonical native Linux gate as an unprivileged tester, without any Go wrapper:5387 passed,0failed,0known, exit0,20m47.313s. Independent hashing verified all2678 tracked blobs againstb97860f. Accepted normally by distinct root manager; both actual source authors remain credited. Production Go/UI, assertions, package selection, baseline and confirming reruns are unchanged. No all-platform hosted CI pass is claimed.
