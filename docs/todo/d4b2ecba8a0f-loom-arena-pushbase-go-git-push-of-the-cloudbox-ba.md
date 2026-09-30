---
id: d4b2ecba8a0f
kind: bug
title: 'loom arena PushBase: go-git push of the cloudbox base to the local loom fails ''repository not found'' (plain git push of the same SHA succeeds)'
seq: 14
status: done
priority: p0
labels:
    - arena
created: 2026-09-30T10:53:16.091005Z
assignee: claude-opus5.5
sprint: 110
sprint_id: 26c12bac-7289-517f-ad46-01e2c117cad7
sprint_title: Revalidate Go 1.27 GNU and POSIX conformance
closed: 2026-09-30T10:58:48.919684Z
closed_by: claude-opus5.5
---

Found 2026-09-30 by the #110 conductor. sprint arena up 110: org sprint-110-26c12bac, repos bashy+cloudbox created, bashy base pushed, cloudbox push returned transport 'repository not found'; coreutils never reached. Plain 'git push http://127.0.0.1:31880/sprint-110-26c12bac/cloudbox.git SHA:refs/heads/base' from the cloudbox checkout succeeds. Workaround used: seed the base with plain git, then arena up (needs the up-to-date fix). Root cause not yet diagnosed (candidates: go-git http push of a large pack / gitea receive-pack response, private submodule origin HEAD).

## Root cause (verified 2026-09-30, conductor #110)

Not the HTTP push. yoke/git installs a process-wide pure-Go "file" transport
whose dotGitLoader chrooted into ".git" unconditionally; in a submodule
checkout (cloudbox, coreutils under the umbrella) ".git" is a "gitdir:" FILE,
so the loader reported transport.ErrRepositoryNotFound for PushBase's local
clone. The same clone passes in a plain go test (stock go-git transport),
which is why it looked host-specific. Fix: follow the gitdir file.
Red/green: yoke/git TestFileTransportFollowsGitdirFile.
