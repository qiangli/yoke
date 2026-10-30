---
id: d95f6b83af5e
kind: task
title: sprint goal link stores the cwd repo path, not the story's register, so a yoke story linked from the umbrella reads 'dangling' forever
seq: 2938
status: done
priority: p1
created: 2026-10-29T22:29:24.956252Z
assignee: muse-conductor
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T23:26:51.151611Z
closed_by: s531-manager
---

Seen on Sprint 531 10-29: goals d1/d2 link 1f7234a6bb83/1d5ec3e23e21 (yoke-register stories, both accepted) but show 'WARNING dangling: $HOME/projects/poc/dhnt#<id>' and stay unchecked. 'bashy sprint goal link 531 d1 --story <id> --repo yoke' (from yoke or the umbrella) answers 'goal d1 already links <id>' and does not correct the stored repo. There's no unlink, and goal evidence doesn't tick the item. Fix: resolve a link by story id across the sprint's tracked repos (or let link --repo overwrite the stored repo for the same id). Red/green unit test in the sprint goal package; focused go test.
