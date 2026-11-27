---
id: 44539a940c1c
kind: bug
title: 'delegate: muse login check times out at 5 s and is reported as not logged in'
seq: 3159
status: todo
priority: p2
created: 2026-11-27T14:25:32.064697Z
---

Found 2026-11-27 (Sprint 738 §1). Under a macOS sandbox, bashy delegate --agent muse-wonder fails with 'muse is not logged in with a subscription' while weave fleet --probe reports USABLE. delegateAuth (pkg/chat/delegate_guard.go:163) runs muse login with a 5-second limit inside the worker env (XDG_CONFIG_HOME / MUSE_CONFIG_DIR under ~/.bashy/workers/muse/config); the muse launcher's overdue self-update and network calls push it past 5 s, and a timeout is reported as 'not logged in'. Fix direction: report a timeout as a timeout, make the limit configurable or larger, and set the vendor's no-auto-update env for the check. Root cause to confirm with: XDG_CONFIG_HOME=... MUSE_CONFIG_DIR=... MUSE_NO_AUTO_UPDATE=1 time muse login under the same sandbox. Not a Sprint 738 story.
