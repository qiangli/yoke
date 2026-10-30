---
id: 9f3fcdde81a1
kind: bug
title: 'S525.A9br yoke: bashy browser not zero-setup (no Edge detection, no provisioning, no persistent session), so A9 real-browser proof impossible on Windows/Linux'
seq: 2929
status: done
priority: p0
labels:
    - software-shaping
created: 2026-10-29T18:13:12.82838Z
weave: 714
assignee: codex-gpt5.6-sol
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T18:26:45.987911Z
resolution: fixed
closed_by: codex-gpt5.6-sol-l
---

Sprint 525, goal a9 (parent eb1be776), p0. A9's acceptance (and A6/A10, plus the real-browser adversarial case carried from A2) needs a REAL browser driving the shaped app. bashy ships `bashy browser`, whose solo mode is documented as "private headless Chrome, zero setup". On the A9 venue it is not zero-setup:
- elephant WSL Ubuntu: `bashy browser navigate ...` -> "no browser: Chrome or Chromium was not found for solo mode (pass --chrome-path)".
- elephant native Windows 11: same, although Microsoft Edge (Chromium) is installed at C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe. pkg/browser/probe/launch.go platformChromePaths() lists only Chrome paths.
Also, solo starts a fresh browser per invocation, so a multi-step UI flow (navigate -> type -> click -> wait-for-selector -> eval) cannot share one page. Probe mode can, but needs a Chrome started by hand with --remote-debugging-port.

Fix (operator rule: zero setup on every OS, any manual step is a bug):
(1) Detection: add Edge (Windows: both Program Files paths; macOS: Microsoft Edge.app; Linux: microsoft-edge, microsoft-edge-stable) and keep Chrome/Chromium.
(2) Provisioning: when no Chromium-family browser is found, bashy provisions a pinned, checksum-verified headless Chromium build (permissively licensed Chromium / chrome-headless-shell) through its existing managed-tool download -> checksum -> cache path (binmgr), per OS/arch, and uses it. No system install, no admin.
(3) Persistent sessions: when probe mode finds no browser at --probe-url, it launches the resolved or provisioned browser headless on that port with the given --user-data-dir (probe.LaunchChrome exists), so a sequence of `bashy browser --mode probe ...` calls drives one page. Add `bashy browser stop` (or reuse an existing verb) to end it.

Acceptance:
- Unit tests for detection (Edge paths per GOOS) and for the provisioning resolver (fake binmgr) and probe auto-launch decision (fake launcher).
- go test ./pkg/browser/... ./cmds/browser/... green; GOOS=windows and GOOS=linux vet.
- No podman, and no real browser download in unit tests. The manager verifies live on a Windows peer and a Linux peer: navigate + type + click + eval across separate calls on one page.
Keep it to pkg/browser and cmds/browser (plus a binmgr tool entry). Sprint/Story/Story-ID trailers; no hostnames.
