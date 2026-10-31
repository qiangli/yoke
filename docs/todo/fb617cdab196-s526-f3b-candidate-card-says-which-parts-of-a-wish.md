---
id: fb617cdab196
kind: bug
title: S526.F3b Candidate card says which parts of a wish it covered and which it did not
seq: 3045
status: todo
priority: p2
created: 2026-10-31T06:55:53.492887Z
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

Operator finding F3 (docs/sprint-526/operator-findings-2026-10-31-wish-page.md): wish 967006fd asked for 3 things; the candidate did 2 and the card did not say so. Fix (yoke pkg/webconsole): the shaping brief (shapingTask in shaped_candidate.go, next to the B5 expand-then-contract guidance) asks the agent to end with a short COVERED/NOT COVERED list against the request; the candidate keeps that summary from the delegate output, and the Shape page card (shape.js, protected) shows it. Acceptance: unit test that the brief asks for the coverage list and that the candidate JSON carries the parsed summary; shape.js renders it. NO podman/docker/make test/full suites on Dragon; focused go test + vet only. Manager does the live check.
