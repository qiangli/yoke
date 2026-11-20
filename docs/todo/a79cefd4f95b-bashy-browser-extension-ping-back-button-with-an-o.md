---
id: a79cefd4f95b
kind: feature
title: 'bashy browser extension: ping-back button with an optional message box'
seq: 3140
status: todo
priority: p1
created: 2026-11-20T13:44:14.501114Z
---

Operator 2026-11-20: in the live-mode extension popup, a 'Send this tab to the agent' button plus an OPTIONAL text box for an additional message. Sends {tab id, url, title, message} to the agent's inbox (bashy mb), so a running session sees it like a steer and can act with --id (or --active). Needs: which agent (default: the live chat session / last addressed agent, selectable), the extension change, and a store re-submission (CWS/Edge). Pairs with --active.

Why (operator, 2026-11-20): a browser the agent launches (`browser --headed login`, solo mode) is a fresh profile with no password manager, so the human cannot sign in there — passwords live in the password manager of their own Edge/Chrome. The flow is therefore human-first: the human launches their own browser (Edge or Chrome, live-mode extension installed), signs in and chooses the tab, then pings back with the button (+ optional message). The agent then acts on exactly that tab over the live hub (`--id` from the ping, or `--active`). Agent-launched login flows should point the human to this path instead of opening a browser they cannot use.

Acceptance: Edge and Chrome; the ping reaches the addressed agent's inbox with tab id/url/title and the optional message; the agent can read that tab (e.g. cookies-get) without the human typing anything into the terminal.
