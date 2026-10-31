---
id: 6cd8a089ad2f
kind: feature
title: 'labels: allow '':'' and ''/'' so labels read naturally and paths work as labels'
seq: 2996
status: todo
priority: p2
labels:
    - todo
created: 2026-10-31T01:44:00.066706Z
---

Operator 2026-10-30: include ':' and '/' in bashy labels so they read naturally (timebox:20m, priority:high, area/ui) and so a PATH can be a label (bashy/examples/tutor, docs/lern-psic). Labels stay a bashy classifier token: no spaces, no commas (comma splits --label a,b), no quoting.
DECIDED (operator delegated 2026-10-30, 'go with your defaults'):
- length: max 128 bytes (was 32).
- case: plain words still lowercase-normalised; a label containing '/' keeps its case (paths are case-sensitive).
- leading '/' allowed (absolute paths); reject any '..' segment, empty segments ('//'), and trailing '/'.
- ':' and '/' allowed after the first character (plus leading '/' as above); kinds share the rule.
Change: pkg/issue/issue.go ValidWord/NormalizeWord + WordRule text + tests (accept: timebox:20m, area/ui, bashy/examples/tutor, /etc/hosts, README-path docs/README.md keeps case; reject: a b, a,b, ../x, a//b, a/, 129-byte label).
Check pkg/feed/follow.go 'label:X' ref still parses (TrimPrefix strips only the leading prefix). Existing labels stay valid.
Follow-up: bashy/examples/tutor (Sprint 547) can switch timebox-20m → timebox:20m after this ships.
