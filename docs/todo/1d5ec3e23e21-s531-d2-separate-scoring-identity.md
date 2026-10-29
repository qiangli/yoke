---
id: 1d5ec3e23e21
kind: feature
title: S531.D2 Separate scoring identity
seq: 2889
status: done
priority: p0
labels:
    - genie
created: 2026-10-29T07:01:15.748395Z
weave: 673
assignee: agy-gemini3.8-flash
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T07:35:21.943815Z
resolution: fixed
closed_by: agy-gemini3.8-flash-w
---

Spec: dhnt umbrella plan §D0 item 5 (decided f7d071321). Depends: D0 (accepted). Repo: yoke (pkg/capability, pkg/ladder).

Scope: delegated outcomes are recorded under a separate identity `genie-delegate:<tool>-<model>` (one colon; e.g. genie-delegate:codex-gpt6-sol) and NEVER merged into genie's own row `ycode:<model>` nor into the vendor's own `tool:model` row.
- Add a small exported helper, e.g. capability.DelegateIdentity(tool, model string) string and a recorder that writes exactly one ladder event for a delegated run under that identity (reuse existing ladder_record/ledger paths; ladderRecordCanonicalAgent must accept it).
- Ensure FamilyID / RatingAgent() resolution cannot fold genie-delegate:* into ycode:* or the vendor family.
- Duty board (ComputeDutyBoard) lists genie-delegate identities as their own rows.
Acceptance: tests in yoke/pkg/capability (+ ladder if touched): a delegated run writes one event under genie-delegate:<..> only; ycode:<model> and vendor rows unchanged (no cross-crediting); board shows the delegate row separately. `go test ./pkg/capability/... ./pkg/ladder/...` green. Public repo: generic commit wording; no hostnames.
