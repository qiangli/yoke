---
id: 6d17e832b8d0
kind: bug
title: chat steer has no sender without USER/LOGNAME (rc.38 blocker)
seq: 3144
status: todo
priority: p1
created: 2026-11-20T16:40:28.957921Z
---

rc.38 gate in the official image: chat steer agy PING failed 'mb: a post needs a sender' — principalName read only BASHY_PRINCIPAL/USER/LOGNAME and the image sets none; queued steers are board posts. Product gap a realistic environment exposed (containers, cron, services). Fix: fall back to USERNAME, then the OS account (user.Current), then uid-<n>.
