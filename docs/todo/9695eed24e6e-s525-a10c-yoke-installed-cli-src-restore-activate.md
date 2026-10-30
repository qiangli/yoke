---
id: 9695eed24e6e
kind: bug
title: 'S525.A10c yoke: installed CLI src restore/activate refuse, so register the production activation gate'
seq: 2918
status: done
priority: p0
labels:
    - software-shaping
created: 2026-10-29T16:25:13.103588Z
weave: 700
assignee: muse-conductor
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T16:37:09.583616Z
resolution: fixed
closed_by: muse-conductor-x
---

Sprint 525, goal a10 (parent 671de6e307ac). Bug found in A9 prep: in the INSTALLED binary, `bashy src restore <app> <rev>` and `bashy src activate <app> <rev>` always refuse with "src: restore refused: no activation gate is wired" (or "activate refused"). src.RegisterActivationGate (pkg/src/activate.go:274) is called only in tests, never in production code (yoke or bashy). A10 requires the real installed CLI to restore and undo, so the CLI must use the same evidence-bound gate the web console uses (productionShapeOptions / the production activator in pkg/webconsole/shaped_candidate.go).

Fix: register the production activation gate once, at init of the package that owns it, so any binary that links the src CLI (bashy embeds yoke) gets it. Avoid an import cycle (pkg/src must not import pkg/webconsole): e.g. move the gate constructor into pkg/shape (or a small pkg/shape/gate) and have both webconsole and the src CLI wiring use it. Keep the gate identical: verify against the current tip, sealed evidence, same refusals.

Acceptance: a focused red/green test that builds the src cobra command the way the binary does (no test-only RegisterActivationGate call). For a principal with 3 activations: `src restore <app> <rev>` succeeds and moves active; restore of a schema-incompatible or gate-failing version refuses with the reason and active unchanged; `src undo` (or the documented undo path) works. go test ./pkg/src ./pkg/shape/... ./pkg/webconsole -run 'TestShaped|TestS525' green. Do not edit pkg/shape/contain (parallel A5w stories). Sprint/Story/Story-ID trailers; no hostnames.
