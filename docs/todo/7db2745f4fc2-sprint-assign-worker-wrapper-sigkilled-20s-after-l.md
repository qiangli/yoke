---
id: 7db2745f4fc2
kind: bug
title: 'sprint assign worker wrapper SIGKILLed ~20s after launch (runs #17/#18/#21); cause unknown'
seq: 3110
status: todo
priority: p1
labels:
    - weave
created: 2026-11-13T00:38:05.44464Z
---

Found by Sprint 627 conductor 2026-11-13. Workers launched by 'bashy sprint assign' (muse #17, #18; agy #21) died 17-21 s after launch: dispatch output 'Killed bashy sprint assign ...', run state failed, result logs stop mid model stream, no reason in 'weave comments'. Not memory pressure (64% free, no jetsam kills). EARLIER HYPOTHESIS WITHDRAWN: 'weave say' was sent to #17/#18 just before they died, but #21 died with no say. Same launch method survived for #15, #19, #22. Also observed: 'weave say' to a headless run reports 'queued for resume; no live delivery'. Next: capture the signal sender (who SIGKILLs the wrapper) on the next occurrence; compare process group / launch parent of surviving vs killed runs.
