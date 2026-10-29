---
id: c02eb8e443c8
kind: refactor
title: 'S525.A1 yoke: unify Bash# skill/script unit model'
seq: 2832
status: todo
priority: p1
created: 2026-10-29T07:15:54.000759Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Part of S525.A1 (umbrella story b976805c404f). Unify Bash# skill and script executable units in yoke. Read first: ../docs/sprint-525-a0-decision-record.md section D-A1 (umbrella checkout = parent dir of this repo; private, do not copy text into this repo).
Scope (yoke only; bashy wiring is a later separate story):
- Canonical unit = SKILL.md with Bash# fences. skill.dhnt is a derived face allowed only when it declares the body identity it derives from; both faces present without matching derivation => refuse as ambiguous (pkg/skills/run.go prepareRun currently silently prefers skill.dhnt).
- One fence extractor in pkg/unit (reuse pkg/dag/parser.go grammar); retire pkg/skills bashSharpFences.
- One identity: pkg/unit/identity for skills, dag, craft and receipts; Bash# receipts fill unit.Attestation and a capability key.
- One contract reader: @require @ensure @guard @effects @confirm @timeout, positional and keyword forms; contract-less units are uncontracted, never verified.
- Migrate consumers: pkg/skills (source, install admit, run, adapt, dagface, promote, export, record, capability, cli), pkg/craft (load, study, compose, resolve, ledger), pkg/lexicon/action.go, testdata/record/dual. Prose-only command docs stay discovery-only.
Acceptance (one focused red/green test): paired-orientation fixture (dhnt face vs Bash# face of the same contract) => same capability key; one source byte change => new identity; ambiguous unit refused; contract-less never verified. go test ./pkg/skills ./pkg/craft ./pkg/lexicon ./pkg/unit ./pkg/dag green. No hostnames or private paths in commits. Commits carry Sprint/Story/Story-ID trailers.
