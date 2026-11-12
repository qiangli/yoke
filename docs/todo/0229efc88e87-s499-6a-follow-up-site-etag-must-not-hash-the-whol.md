---
id: 0229efc88e87
kind: bug
title: 'S499.6A follow-up: site ETag must not hash the whole file on every (Range) request'
seq: 3104
status: todo
priority: p2
labels:
    - app
    - web
    - perf
created: 2026-11-12T17:56:24.326162Z
sprint: 499
sprint_id: c3c45f32-63b5-5575-980f-299a44a561f3
sprint_title: 'Content creation with bashy: YC videos and a local-first author-to-reader showcase'
---

Found in review of S499.6A (merged yoke 40d456db): siteHandler (pkg/webconsole/site.go) computes a sha256 of the WHOLE file on every request before http.ServeContent, including every Range request. Seeking in a 1 GB video re-reads 1 GB per seek.
- Classification: product defect (performance). Not a security issue.
- Fix: use a validator that costs nothing per request, a weak ETag from size + mtime (W/"<size>-<mtime-ns>"). Or cache the strong hash keyed by (path, size, mtime) and compute it once.
- When bashy-meta-v1 lands (render reuse), a recorded content key becomes the strong ETag.
- Tests:
  - a Range request does not read the full file (counting reader or a large sparse fixture with a time bound)
  - If-None-Match → 304
  - the ETag changes after an edit
