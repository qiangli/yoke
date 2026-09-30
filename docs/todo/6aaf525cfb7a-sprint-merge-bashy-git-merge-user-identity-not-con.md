---
id: 6aaf525cfb7a
kind: bug
title: 'sprint merge / bashy git merge: ''user identity not configured'' in a checkout whose identity is only in ~/.gitconfig'
seq: 16
status: todo
priority: p0
labels:
    - git
created: 2026-09-30T11:16:19.915867Z
sprint: 110
sprint_id: 26c12bac-7289-517f-ad46-01e2c117cad7
sprint_title: Revalidate Go 1.27 GNU and POSIX conformance
---

Found 2026-09-30 merging Sprint #110 run coreutils#2. yoke/git commitSignature used go-git ConfigScoped(LocalScope), which is the repo config ALONE (the code comment had the scope order backwards); SystemScope is the merged view. Red/green: yoke/git TestCommitSignatureFallsBackToGlobalConfig (global fallback + repo-local precedence).
