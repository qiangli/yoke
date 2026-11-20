---
id: fe274376f7da
kind: chore
title: yoke fmtcheck fails on existing files (cmd/yoke/main.go, cmds/winpath/*, pkg/fleet/apps.go, ...) so the pre-push gate fails for everyone
seq: 3134
status: todo
priority: p2
labels:
    - tests
created: 2026-11-20T03:41:07.042266Z
sprint: 676
sprint_id: cc17f40d-d5ff-5d27-bbed-b8563759cde6
sprint_title: 'Fleet hardening after the tool-shortcut tests: steerable ycode/zcode/dsh + pre-existing test failures'
---

Seen 2026-11-19: scripts/fmtcheck.sh exit 1 on files unchanged vs origin/main. Likely gofmt version drift (dev host go1.27.2) or doc-comment reflow; the script header says the fix is a decision, not an automation. Determine cause first; then reformat with the repo's pinned toolchain only if the change is whitespace/doc-comment formatting with no behaviour change.
