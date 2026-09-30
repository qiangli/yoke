---
id: 959d7a40b547
kind: bug
title: 'sprint commit-msg resolves Story: #N by number, so per-repo number collisions (and a duplicate seq) make valid stories uncommittable'
seq: 11
status: todo
priority: p0
labels:
    - weave
created: 2026-09-30T10:34:36.680513Z
sprint: 332
sprint_id: 5591ec3f-fa56-5047-a968-cd01097cee2a
sprint_title: 'bashy small improvements and bug fixes — #314 follow-up'
---

Found by the steward 2026-09-30 committing Sprint #100 story reviews: sh 38c49bfdb5d3 is #3 and coreutils 8a64e0c1b37d is also #3 (numbers are per repo, sprint #100 tracks both); coreutils holds two #15 (6972ef8ec6cb, 95fcce29a7cc). The hook rejected them ('Story: #3 resolves to 8a64e0c1b37d, not Story-ID: 38c49bfdb5d3'; 'duplicate story provenance pair #15'). Blocks the POSIX conductor from committing fixes for those stories. Fix: resolve by Story-ID (the identity), require the number to equal that story's own seq, refuse duplicates by id only. Red/green: TestValidateCommitTraceStoriesSharedNumber.
