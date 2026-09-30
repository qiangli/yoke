---
id: 33c289c94fe9
kind: bug
title: 'weave booth: keychain-backed logins (muse on macOS) fail in a booth HOME - ''missing meta credentials'''
seq: 15
status: todo
priority: p0
labels:
    - booth
created: 2026-09-30T11:13:09.317892Z
sprint: 110
sprint_id: 26c12bac-7289-517f-ad46-01e2c117cad7
sprint_title: Revalidate Go 1.27 GNU and POSIX conformance
---

Found 2026-09-30 in Sprint #110 heat ee78ee97: muse-spark1.3 entrant exited in 1s with 'missing meta credentials'. ~/.config/muse/auth.json was seeded correctly but says storage=keychain; the macOS login keychain is resolved via $HOME/Library/Keychains, which a booth HOME lacks. Verified live: linking booth-home/Library/Keychains to the real dir makes muse answer from the booth. Fix: boothSeedAgentDirs links it. Red/green TestBoothHomeReachesLoginKeychain; go test ./pkg/weave green (278s).
