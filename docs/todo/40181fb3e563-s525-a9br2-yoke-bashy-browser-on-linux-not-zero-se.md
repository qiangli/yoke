---
id: 40181fb3e563
kind: bug
title: 'S525.A9br2 yoke: bashy browser on Linux not zero-setup: provisioned chrome-headless-shell cannot start on minimal Ubuntu (missing libnspr4/libnss3/libnssutil3/libasound2; root also needs --no-sandbox)'
seq: 2931
status: done
priority: p0
labels:
    - linux
    - browser
created: 2026-10-29T19:05:36.893204Z
weave: 722
assignee: codex-gpt5.6-sol
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T19:25:26.966229Z
resolution: fixed
closed_by: codex-gpt5.6-sol-t
---

Found by A9 run 4 on elephant WSL Ubuntu 24.04 (glibc 2.39), candidate bashy dc64c42b / yoke 8694ad81. 'bashy browser --mode probe navigate URL' -> 'probe: browser launched but http://127.0.0.1:9335 did not become ready'. Running the provisioned binary (~/.cache/bashy/bin/chrome-headless-shell/156.0.8078.73/...) directly: 'error while loading shared libraries: libnspr4.so'. ldd not found: libnspr4.so libnss3.so libnssutil3.so libasound.so.2. Process runs as root (uid 0), so Chromium will also refuse without --no-sandbox. Windows native is fine (Edge detected; navigate/eval/stop OK).
Fix (minimal, zero setup, no new framework; operator rule: no host workarounds, no apt by hand): (1) launcher passes --no-sandbox when euid==0 on Linux (Chromium's own sandbox, not a bashy sandbox); (2) when the resolved browser has unresolved shared libs on Linux, binmgr provisions the missing runtime libs as sha256-pinned downloads into the same cache dir and the launcher sets LD_LIBRARY_PATH for that child only (download+exec, never vendored into bashy; note NSS is MPL-2.0 / alsa-lib LGPL in the commit so the operator can veto); OR pick an equally pinned self-contained headless Chromium build if simpler. (3) Launch failure must surface the child's stderr (it currently hides the real cause behind 'did not become ready').
Test: unit test with fake launcher/binmgr for root flag + lib-path env + stderr surfacing; GOOS=linux/windows vet. Live proof by the manager on elephant WSL (A9 browser phase).
