---
id: 8a88f8a338bd
kind: bug
title: 'weave start --resume stalls for minutes: weaveResumePrompt calls Post.Directed (room.Members card reads) on every bus post before the date filter'
seq: 3152
status: todo
priority: p2
created: 2026-11-24T13:55:23.292594Z
---

Observed 2026-11-24 (Sprint 691 run 234): weave start --resume --run 234 [--handoff-to X] printed nothing for >5 min. SIGQUIT dump: goroutine 1 in pkg/weave/weave_steering.go:60 weaveResumePrompt -> bus.Post.Directed -> RegisterHostRoles -> conductorRoles -> SprintLeaseEffectiveHeartbeat -> room.OwnerTransportFor -> room.Members -> readCard (os.ReadFile per call). bus.Posts() walks the whole local timeline (59 MB on the dev box), and Directed() runs before the cheap 'at.Before(it.Created)' filter, so every post re-reads member cards. Classification: product defect (performance), not venue. Fix: apply the time filter first and snapshot role/member resolution once per call. Verify: resume with a large timeline returns in seconds.
