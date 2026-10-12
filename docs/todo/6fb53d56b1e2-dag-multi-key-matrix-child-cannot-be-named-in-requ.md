---
id: 6fb53d56b1e2
kind: bug
title: 'dag: multi-key Matrix child cannot be named in Requires (splitList splits on commas)'
seq: 1885
status: done
priority: p2
labels:
    - dag
created: 2026-10-11T06:53:41.551831Z
weave: 188
assignee: s9999-dag
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T14:57:03.638741Z
closed_by: s9999-maintenance-manager
---

Bug split out of deferred story 853c292b4ab2 (2026-10-10 rescope). yoke pkg/dag/parser.go splitList splits Requires on commas, so a multi-key matrix child such as build:arch=amd64,os=linux cannot be named in Requires; single-key children only. Fix: do not split inside a target:key=v,key2=v2 child reference. Acceptance: red/green parser test naming a two-key child in Requires; existing comma-separated Requires lists unchanged.
