---
id: fddf853b9a39
kind: feature
title: 'outpost default on every platform: disable host sleep and run as a boot-surviving service (Windows/macOS/Linux)'
seq: 2040
status: todo
priority: p0
created: 2026-10-23T19:27:26.475067Z
sprint: 495
sprint_id: d7c905ee-3395-5fb3-82aa-b1585a02530e
sprint_title: 'Seamless onboarding: bashy onboard, setup-recipe rods, install/signing/winget'
---

Operator rule 2026-10-23: every outpost turns off sleep mode and survives reboot by default, on all supported platforms (Windows, macOS, Linux). Triggered by elephant (Windows 11) repeatedly dropping off the LAN (sleep, then APIPA without a DHCP lease) and by bootstrap sshd being foreground-only.
Deliver in the outpost install/onboard path (the setup recipe + service verbs), idempotent, and reported in doctor:
- Windows: `powercfg /change standby-timeout-ac 0` and `-dc 0`, `hibernate-timeout-ac/dc 0`, disable hibernate and network-adapter power-off where possible; outpost and outpost.sshd as boot services (Task Scheduler at startup or a Windows service) that run without a logged-in user.
- macOS: `pmset -a sleep 0 disksleep 0 womp 1` (keep displaysleep), plus a LaunchDaemon (or a LaunchAgent with KeepAlive/RunAtLoad), so it survives logout and reboot.
- Linux: mask sleep.target, suspend.target, hibernate.target and hybrid-sleep.target (and set logind IdleAction=ignore), plus a systemd user unit with `loginctl enable-linger` (or a system unit).
Each change needs admin rights: request them through bashy ask, never store them; report clearly when a step cannot be applied. Doctor shows sleep: disabled|enabled and service: boot|session|none. Acceptance: on each of the three OSes, a fresh install leaves sleep disabled and the service enabled, and a reboot brings outpost back online without a login. Repo: the owning outpost/yoke recipe code (split per repo when staffing).

POST-INSTALL MUST-CHECK (operator 2026-10-23, all platforms): install/onboard is not done until a post-install check passes:
(a) wake: the host's sleep/standby/hibernate settings read back as disabled, and a no-sleep probe (no suspend events over the check window, or OS power-settings readback where that is the only signal) passes;
(b) reboot: a controlled reboot, then outpost (and outpost.sshd where installed) is online again on its server and reachable through the tunnel and the LAN sshd, without anyone logging in, within a bounded time.
The check is an agent-runnable, idempotent recipe step (e.g. `bashy outpost verify --post-install`, or the setup recipe's final dag step), records PASS/FAIL with raw evidence, and runs on Windows, macOS and Linux. A FAIL is reported, never silently ignored. Hosts the operator marks production (e.g. lern) get the reboot step only with explicit operator approval.
