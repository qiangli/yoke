---
id: bb6640fa2bdc
kind: bug
title: weave abandon --force preservation commit fails the commit-msg hook
seq: 2027
status: todo
labels:
    - weave
created: 2026-10-18T18:34:52.862559Z
---

Observed 2026-10-18 on installed bashy b1485db4 (yoke 52e2e4f4): weave abandon 365 --disposition superseded --yes refused: --force could not commit 1 uncommitted file(s) for preservation; sprint commit-msg: final trailer paragraph contains a non-trailer line (This tree was committed by --force so it could be preserved as a ...). The only dirty file was the weave-generated KB.md. Workaround used: discard the generated KB.md change, then abandon succeeds and preserves unmerged tips. Fix: give the preservation commit a hook-valid message (or skip the hook for this internal commit) and exclude the generated KB.md, with a regression test.
