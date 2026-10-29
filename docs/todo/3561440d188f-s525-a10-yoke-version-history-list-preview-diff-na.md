---
id: 3561440d188f
kind: feature
title: 'S525.A10 yoke: version history list/preview/diff/name/restore'
seq: 2897
status: done
priority: p1
created: 2026-10-29T08:00:58.420318Z
weave: 678
assignee: muse-conductor
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T08:07:41.059737Z
resolution: fixed
closed_by: muse-conductor-b
---

Part of S525.A10 (umbrella story 671de6e307ac). Version history for shaped apps, in yoke. Builds directly on S525.A7 (pkg/src/activate.go version lines/named/review refs, pkg/shape/state store + ActivationGate, src activate, weave start --base). Public docs and commit messages describe user-visible behavior only, not the internal mechanism.
Behavior (like an online document history, per principal):
- `bashy src history <app> [--user P]`: list versions on the principal line: who, when, the request text, verifier verdict, evidence link (from the commit trailers A7 writes), plus named labels and review versions. --json too.
- `bashy src preview <app> <rev>`: prepare an isolated workspace at that revision (reuse the weave --base path or the same clone helper); the active revision does not move. Print where it is / how to open it.
- `bashy src diff <app> <a> <b>`: diff any two versions of that principal (or base).
- `bashy src name <app> <rev> <label>`: name a version (A7 named ref helper).
- `bashy src restore <app> <rev>`: create a NEW forward commit on the principal line whose tree equals <rev> (history never rewritten), pass it through the same ActivationGate, then activate. Undo = restoring the pre-restore version. Restore changes code/config only; the principal data dir is untouched.
- Schema guard: feature metadata (pkg/shape/state feature reader) carries a schema range; the principal state records the current data schema. If the target version cannot read the current data schema, restore is refused with an explanation. A gate refusal (e.g. evidence fails current checks) also refuses restore and leaves the active revision unchanged.
- Another principal history and active revision are never affected.
- Leave an exported Go API (History, Restore, Name, Diff) the launcher UI will call next.
Acceptance (one focused red/green test on a temp src store with two principals): activate 4 versions for alice, restore 3 back, undo the restore, name one, diff two; bob unchanged; schema-incompatible restore refused; gate-refused restore leaves active unchanged; data dir untouched. go test ./pkg/src -run <your tests> green; existing ./pkg/src suite green. Sprint/Story/Story-ID trailers; no hostnames. Do NOT edit pkg/shape/verify, pkg/shape/evidence, pkg/src/pr.go (S525.A2 running) or pkg/webconsole (S525.A6 running).
