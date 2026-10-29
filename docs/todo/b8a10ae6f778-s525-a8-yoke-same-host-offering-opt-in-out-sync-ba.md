---
id: b8a10ae6f778
kind: feature
title: 'S525.A8 yoke: same-host offering, opt-in/out, sync (backend+CLI)'
seq: 2898
status: done
priority: p1
created: 2026-10-29T08:08:31.11072Z
weave: 679
assignee: agy-gemini3.8-flash
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T08:23:44.801523Z
resolution: fixed
closed_by: agy-gemini3.8-flash-c
---

Part of S525.A8 (umbrella story dde1517998ed). Same-host offering, opt-in/opt-out and sync for shaped apps — backend + CLI in yoke. Builds on S525.A7/A10 on main: pkg/src/activate.go (per-principal version lines, named and review refs, Activate through an ActivationGate), pkg/src/history.go (History/Restore/Name/Diff API), pkg/shape/state (per-principal store: active revision, last-known-good, adopted items, notices). Public docs and commit messages describe user-visible behavior only, not the internal mechanism.
Behavior:
- `bashy src offer <app> <rev> [--to P,...|--all]`: the creator publishes a revision of their feature as a host offering. The offering carries ONLY the source change (data and private config are never in the tree, so nothing personal travels) plus origin principal, base revision and dependencies. Restricted offerings are invisible to principals not listed.
- Eligible principals get ONE notice per offering revision in their state (no duplicates on re-offer of the same revision).
- `bashy src offers <app>` lists offerings visible to the principal; `bashy src adopt <app> <offer> [--preview]`: preview first (isolated workspace, active untouched), then merge the offering revision into the principal own line and activate through the same ActivationGate. `decline` records the decision (notice cleared). Disable/rollback = existing restore; data is never erased.
- Sync: when the creator offers a later revision, adopters get a notice and receive it ONLY on acceptance (adopt again); a conflict with the adopter own changes becomes a review version for that adopter (existing review ref helper) and the active revision does not move. Non-adopters stay unchanged. Withdrawal of an offering does not touch adopters.
- Adoption never copies the creator credentials or data.
- Export a Go API (Offer, ListOffers, Adopt, Decline, Notices) the launcher UI will call.
Acceptance (one focused red/green test on a temp src store, three principals): alice offers to bob only; carol cannot list it; bob previews then adopts (active changes, data untouched); alice offers revision 2; bob receives it only after adopting again; a conflicting change by bob yields a review version with active unchanged; decline clears the notice; carol unchanged throughout. go test ./pkg/src -run <your test> green; existing ./pkg/src suite green. Do NOT edit pkg/webconsole (S525.A6), pkg/shape/verify, pkg/shape/evidence, pkg/src/pr.go, pkg/src/gate.go (S525.A2). Sprint/Story/Story-ID trailers; no hostnames.
