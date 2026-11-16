---
id: 105a358a2fe3
kind: bug
title: 'browser live: hub flaps between Chrome profiles (connect/disconnect/hello loop)'
seq: 3115
status: done
priority: p1
labels:
    - browser
created: 2026-11-16T10:32:50.593784Z
closed: 2026-11-16T10:36:32.133426Z
---

Symptom: bashy browser hub logs 'extension connected' / 'extension disconnected' / 'extension hello' in the same millisecond, repeating; two hellos alternate (methods=22 and version=0.10.1 methods=28). Operator workaround: disconnect the extension in every Chrome profile, reconnect one.

Root cause (product defect, recorded 2026-11-16): each Chrome profile runs its own copy of the extension and keeps its connect intent. The hub holds ONE connection and the newest one wins (server.go handleWS closes the prior conn). The evicted extension's onclose schedules a reconnect at RECONNECT_BASE_MS (1s), and onopen resets the backoff, so two connected profiles evict each other forever. An older copy can also trigger the hub's hot-reload path. All connections come from 127.0.0.1, so the log cannot tell profiles apart.

Fix: the hub refuses an automatic reconnect while a live connection exists (close code 4409 'another profile is connected'); an explicit popup Connect carries a takeover flag and may still replace it. The extension stops its reconnect loop on 4409 and the popup says why. Log the connection's version/instance on connect.
