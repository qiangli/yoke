---
id: 7bc5d2685915
kind: bug
title: S557 use portable temporary homes in native test entrypoints
seq: 3046
status: assigned
priority: p1
created: 2026-11-01T17:09:03.38808Z
assignee: s557-public-tooling
sprint: 557
sprint_id: f5ccf563-f5d1-5b94-8be4-9388e51ef86d
sprint_title: 'Bashy v1.0.0 preparation: split private/public development and retire qiangli/dhnt'
---

Yoke chat/supervise TestMain uses hardcoded /tmp unavailable on Windows. Use os.MkdirTemp empty base, preserve HOME and USERPROFILE isolation and cleanup before os.Exit. Verify bounded native cases and Windows compile; do not skip packages.

Delivery candidate: chat/supervise TestMain now allocate homes and shim roots
with os.MkdirTemp's platform default, retaining HOME/USERPROFILE isolation.
Supervise now cleans both roots after m.Run and before os.Exit, matching chat.
Validation: exact TestMain bodies extracted into isolated stdlib-only fixtures
passed native isolation and post-exit cleanup checks, and Windows/amd64 test
compilation. Full package suites remain hosted validation work. Manager accepts.
